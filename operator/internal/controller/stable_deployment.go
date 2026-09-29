package controller

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	// stableDeploymentName is the stable api Deployment's name. Adoption keeps
	// it: the operator takes over the live deployment/api in place rather than
	// replacing it with a Deployment of its own (docs/PLAN.md D31).
	stableDeploymentName = "api"
	// canaryDeploymentName is the canary api Deployment the operator creates,
	// owns and scales for a canary window (docs/PLAN.md D32, D37).
	canaryDeploymentName = "api-canary"
	// apiContainerName is the container whose image the operator sets.
	apiContainerName = "api"
)

// DefaultImagePrefix is apply.sh's default IMAGE_PREFIX: the registry and
// owner GhcrPublish pushes the per-SHA images to. It is the operator's
// --image-prefix default; apply.sh renders its own IMAGE_PREFIX into that flag
// in 03-operator.yaml, so a k3d rehearsal that imports local builds under
// docker.io/library has the operator write those same references.
const DefaultImagePrefix = "ghcr.io/muratalkan06"

// apiImage renders an image tag as the api image reference under prefix, in
// the form deploy/k3s/apply.sh renders <prefix>/mlobs-api:<tag>. Every image
// the operator writes comes from here.
func apiImage(prefix, tag string) string {
	return prefix + "/mlobs-api:" + tag
}

// apiLabels returns a fresh copy of the stable api's labels, so the
// Deployment's metadata, selector and pod template never share one map. They
// are the labels the live deployment/api has always carried; adoption must
// not change its template, and its selector is immutable, so they stay as
// they are.
func apiLabels() map[string]string {
	return map[string]string{"app": "api"}
}

// canaryLabels returns a fresh copy of the canary's labels. app: api puts the
// canary pods behind the shared api Service, whose selector is app: api —
// that is the D34 per-connection split. role: canary is what the api-canary
// Service selects, and it keeps the canary's own selector clear of the stable
// pods. The stable's selector does match the canary pods; the Deployment and
// ReplicaSet controllers tolerate that overlap because each only counts pods
// and ReplicaSets whose controller reference points at itself (operator
// README, "Labels").
func canaryLabels() map[string]string {
	return map[string]string{"app": "api", "role": "canary"}
}

// newStableDeployment returns the api Deployment the operator creates when
// deployment/api is absent.
//
// Since O2 no manifest renders deployment/api, so this is the one description
// of its shape that ships. testdata/api-deployment.yaml holds the same
// Deployment as YAML, with the reasoning behind each field (Recreate, the
// three probes, the D8 memory bounds) carried over from the 20-api.yaml
// Deployment it replaces, and a test fails on any difference between the two:
// a change here is a change to both. There is no hostPort: the api is
// published by the NodePort Service in 20-api.yaml (D34 and the D19 erratum of
// O2). An adopted Deployment is never rebuilt from this shape; adoption
// changes only its ownerReferences and its api image (D31).
func newStableDeployment(namespace, image string) *appsv1.Deployment {
	return newAPIDeployment(stableDeploymentName, namespace, apiLabels, image, 1)
}

// newCanaryDeployment returns deployment/api-canary at image and replicas: the
// stable's pod template under the canary's labels, so that it runs beside the
// stable on one node behind the shared Service.
func newCanaryDeployment(namespace, image string, replicas int32) *appsv1.Deployment {
	return newAPIDeployment(canaryDeploymentName, namespace, canaryLabels, image, replicas)
}

// newAPIDeployment returns an api Deployment named name, labelled, selected
// and templated with labels(), running image at replicas.
func newAPIDeployment(
	name, namespace string, labels func() map[string]string, image string, replicas int32,
) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels(),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: new(replicas),
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: labels()},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels()},
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{{
						Name:    "redis-ready",
						Image:   "redis:7.4-alpine",
						Command: []string{"sh", "-c", "until redis-cli -h redis ping; do sleep 2; done"},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("16Mi")},
						},
					}},
					Containers: []corev1.Container{{
						Name:            apiContainerName,
						Image:           image,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Ports: []corev1.ContainerPort{{
							Name:          "http",
							ContainerPort: 8000,
						}},
						Env: []corev1.EnvVar{{Name: "REDIS_URL", Value: "redis://redis:6379/0"}},
						StartupProbe: &corev1.Probe{
							ProbeHandler:     healthProbeHandler(),
							PeriodSeconds:    10,
							TimeoutSeconds:   5,
							FailureThreshold: 18,
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler:     healthProbeHandler(),
							PeriodSeconds:    10,
							TimeoutSeconds:   5,
							FailureThreshold: 3,
						},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(8000)},
							},
							PeriodSeconds:    20,
							TimeoutSeconds:   5,
							FailureThreshold: 3,
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
							Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1536Mi")},
						},
					}},
				},
			},
		},
	}
}

// healthProbeHandler is the GET /health handler the startup and readiness
// probes share; each call returns its own copy.
func healthProbeHandler() corev1.ProbeHandler {
	return corev1.ProbeHandler{
		HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromInt32(8000)},
	}
}
