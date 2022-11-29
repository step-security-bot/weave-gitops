package server_test

import (
	"context"
	"fmt"
	"testing"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1beta2"
	. "github.com/onsi/gomega"
	"github.com/weaveworks/weave-gitops/core/clustersmngr/cluster"
	"github.com/weaveworks/weave-gitops/core/server"
	stypes "github.com/weaveworks/weave-gitops/core/server/types"
	pb "github.com/weaveworks/weave-gitops/pkg/api/core"
	"github.com/weaveworks/weave-gitops/pkg/kube"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestGetReconciledObjects(t *testing.T) {
	g := NewGomegaWithT(t)

	ctx := context.Background()

	c, _ := makeGRPCServer(k8sEnv.Rest, t)

	scheme, err := kube.CreateScheme()
	g.Expect(err).To(BeNil())

	k, err := client.New(k8sEnv.Rest, client.Options{
		Scheme: scheme,
	})
	g.Expect(err).NotTo(HaveOccurred())

	automationName := "my-automation"
	ns := newNamespace(ctx, k, g)

	reconciledObj := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-deployment",
			Namespace: ns.Name,
			Labels: map[string]string{
				server.KustomizeNameKey:      automationName,
				server.KustomizeNamespaceKey: ns.Name,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": automationName,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": automationName},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "nginx",
						Image: "nginx",
					}},
				},
			},
		},
	}

	g.Expect(k.Create(ctx, &reconciledObj)).Should(Succeed())

	res, err := c.GetReconciledObjects(ctx, &pb.GetReconciledObjectsRequest{
		AutomationName: automationName,
		Namespace:      ns.Name,
		AutomationKind: kustomizev1.KustomizationKind,
		Kinds:          []*pb.GroupVersionKind{{Group: "apps", Version: "v1", Kind: "Deployment"}},
		ClusterName:    cluster.DefaultCluster,
	})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Objects).To(HaveLen(1))

	first := res.Objects[0]
	g.Expect(first.Payload).To(ContainSubstring("Deployment"))
	g.Expect(first.Payload).To(ContainSubstring(reconciledObj.Name))
}

func TestGetReconciledObjectsWithSecret(t *testing.T) {
	g := NewGomegaWithT(t)

	ctx := context.Background()

	c, _ := makeGRPCServer(k8sEnv.Rest, t)

	scheme, err := kube.CreateScheme()
	g.Expect(err).To(BeNil())

	k, err := client.New(k8sEnv.Rest, client.Options{
		Scheme: scheme,
	})
	g.Expect(err).NotTo(HaveOccurred())

	automationName := "my-automation"
	ns := newNamespace(ctx, k, g)

	reconciledObj := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-secret",
			Namespace: ns.Name,
			UID:       "this-is-not-an-uid",
			Labels: map[string]string{
				server.KustomizeNameKey:      automationName,
				server.KustomizeNamespaceKey: ns.Name,
			},
		},
		TypeMeta: metav1.TypeMeta{Kind: "Secret", APIVersion: "v1"},
		Data:     map[string][]byte{"username": []byte("username"), "password": []byte("password")},
	}

	g.Expect(k.Create(ctx, &reconciledObj)).Should(Succeed())

	res, err := c.GetReconciledObjects(ctx, &pb.GetReconciledObjectsRequest{
		AutomationName: automationName,
		Namespace:      ns.Name,
		AutomationKind: kustomizev1.KustomizationKind,
		Kinds:          []*pb.GroupVersionKind{{Group: "", Version: "v1", Kind: "Secret"}},
		ClusterName:    cluster.DefaultCluster,
	})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Objects).To(HaveLen(1))

	first := res.Objects[0]
	g.Expect(first.Payload).To(ContainSubstring("redacted"))
}

func TestGetChildObjects(t *testing.T) {
	g := NewGomegaWithT(t)

	ctx := context.Background()

	automationName := "my-automation"

	ns := corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-namespace",
		},
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-deployment",
			Namespace: ns.Name,
			UID:       "this-is-not-an-uid",
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": automationName,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": automationName},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "nginx",
						Image: "nginx",
					}},
				},
			},
		},
	}

	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-123abcd", automationName),
			Namespace: ns.Name,
		},
		Spec: appsv1.ReplicaSetSpec{
			Template: deployment.Spec.Template,
			Selector: deployment.Spec.Selector,
		},
		Status: appsv1.ReplicaSetStatus{
			Replicas: 1,
		},
	}

	rs.SetOwnerReferences([]metav1.OwnerReference{{
		UID:        deployment.UID,
		APIVersion: appsv1.SchemeGroupVersion.String(),
		Kind:       "Deployment",
		Name:       deployment.Name,
	}})

	scheme, err := kube.CreateScheme()
	g.Expect(err).To(BeNil())

	client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(&ns, deployment, rs).Build()
	cfg := makeServerConfig(client, t)
	c := makeServer(cfg, t)

	res, err := c.GetChildObjects(ctx, &pb.GetChildObjectsRequest{
		ParentUid: string(deployment.UID),
		Namespace: ns.Name,
		GroupVersionKind: &pb.GroupVersionKind{
			Group:   "apps",
			Version: "v1",
			Kind:    "ReplicaSet",
		},
		ClusterName: cluster.DefaultCluster,
	})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Objects).To(HaveLen(1))

	first := res.Objects[0]
	g.Expect(first.Payload).To(ContainSubstring("ReplicaSet"))
	g.Expect(first.Payload).To(ContainSubstring(rs.Name))
}

func TestListFluxRuntimeObjects(t *testing.T) {
	g := NewGomegaWithT(t)

	ctx := context.Background()

	tests := []struct {
		description string
		objects     []runtime.Object
		assertions  func(*pb.ListFluxRuntimeObjectsResponse)
	}{
		{
			"no flux runtime",
			[]runtime.Object{
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns1"}},
			},
			func(res *pb.ListFluxRuntimeObjectsResponse) {
				g.Expect(res.Errors[0].Message).To(Equal(server.ErrFluxNamespaceNotFound.Error()))
				g.Expect(res.Errors[0].Namespace).To(BeEmpty())
				g.Expect(res.Errors[0].ClusterName).To(Equal(cluster.DefaultCluster))
			},
		},
		{
			"flux namespace label, with controllers",
			[]runtime.Object{
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "flux-ns", Labels: map[string]string{
					stypes.PartOfLabel: server.FluxNamespacePartOf,
				}}},
				newDeployment("random-flux-controller", "flux-ns", map[string]string{stypes.PartOfLabel: server.FluxNamespacePartOf}),
				newDeployment("other-controller-in-flux-ns", "flux-ns", map[string]string{}),
			},
			func(res *pb.ListFluxRuntimeObjectsResponse) {
				g.Expect(res.Deployments).To(HaveLen(1), "expected deployments in the flux namespace to be returned")
				g.Expect(res.Deployments[0].Name).To(Equal("random-flux-controller"))
			},
		},
		{
			"use flux-system namespace when no namespace label available",
			[]runtime.Object{
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "flux-system"}},
				newDeployment("random-flux-controller", "flux-system", map[string]string{stypes.PartOfLabel: server.FluxNamespacePartOf}),
				newDeployment("other-controller-in-flux-ns", "flux-system", map[string]string{}),
			},
			func(res *pb.ListFluxRuntimeObjectsResponse) {
				g.Expect(res.Deployments).To(HaveLen(1), "expected deployments in the default flux namespace to be returned")
				g.Expect(res.Deployments[0].Name).To(Equal("random-flux-controller"))
			},
		},
		{
			"don't use flux-system if other namespace has better labels",
			[]runtime.Object{
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "flux-system"}},
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "flux-ns", Labels: map[string]string{
					stypes.PartOfLabel: server.FluxNamespacePartOf,
				}}},
				newDeployment("correct-flux-controller", "flux-ns", map[string]string{stypes.PartOfLabel: server.FluxNamespacePartOf}),
				newDeployment("incorrect-flux-controller", "flux-system", map[string]string{stypes.PartOfLabel: server.FluxNamespacePartOf}),
			},
			func(res *pb.ListFluxRuntimeObjectsResponse) {
				g.Expect(res.Deployments).To(HaveLen(1), "expected deployments in flux ns to be returned")
				g.Expect(res.Deployments[0].Name).To(Equal("correct-flux-controller"))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.description, func(t *testing.T) {
			scheme, err := kube.CreateScheme()
			g.Expect(err).To(BeNil())
			client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(tt.objects...).Build()
			cfg := makeServerConfig(client, t)
			c := makeServer(cfg, t)
			res, err := c.ListFluxRuntimeObjects(ctx, &pb.ListFluxRuntimeObjectsRequest{})
			g.Expect(err).NotTo(HaveOccurred())

			tt.assertions(res)
		})
	}
}

func newDeployment(name, ns string, labels map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": name},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "nginx",
						Image: "nginx",
					}},
				},
			},
		},
	}
}

func TestListFluxCrds(t *testing.T) {
	g := NewGomegaWithT(t)

	ctx := context.Background()

	crd1 := &apiextensions.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{
		Name:   "crd1",
		Labels: map[string]string{stypes.PartOfLabel: "flux"},
	}, Spec: apiextensions.CustomResourceDefinitionSpec{
		Group:    "group",
		Names:    apiextensions.CustomResourceDefinitionNames{Plural: "plural", Kind: "kind"},
		Versions: []apiextensions.CustomResourceDefinitionVersion{},
	}}
	crd2 := &apiextensions.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{
		Name:   "crd2",
		Labels: map[string]string{stypes.PartOfLabel: "flux"},
	}, Spec: apiextensions.CustomResourceDefinitionSpec{
		Group:    "group",
		Versions: []apiextensions.CustomResourceDefinitionVersion{{Name: "0"}},
	}}
	scheme, err := kube.CreateScheme()
	g.Expect(err).To(BeNil())

	client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(crd1, crd2).Build()
	cfg := makeServerConfig(client, t)
	c := makeServer(cfg, t)

	res, err := c.ListFluxCrds(ctx, &pb.ListFluxCrdsRequest{})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Crds).To(HaveLen(2))

	first := res.Crds[0]
	g.Expect(first.Version).To(Equal(""))
	g.Expect(first.Name.Plural).To(Equal("plural"))
	g.Expect(first.Name.Group).To(Equal("group"))
	g.Expect(first.Kind).To(Equal("kind"))
	g.Expect(first.ClusterName).To(Equal(cluster.DefaultCluster))
	g.Expect(res.Crds[1].Version).To(Equal("0"))
}
