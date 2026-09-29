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
	// apiContainerName is the container whose image the operator sets.
	apiContainerName = "api"
	// imagePrefix is apply.sh's default IMAGE_PREFIX: the registry and owner
	// GhcrPublish pushes the per-SHA images to.
	imagePrefix = "ghcr.io/muratalkan06"
)

// stableImage renders an image tag as the api image reference, in the form
// deploy/k3s/apply.sh renders IMAGE_PREFIX/mlobs-api:IMAGE_TAG under its
// default prefix. Every image the operator writes comes from here.
func stableImage(tag string) string {
	return imagePrefix + "/mlobs-api:" + tag
}

// apiLabels returns a fresh copy of the api's labels, so the Deployment's
// metadata, selector and pod template never share one map.
func apiLabels() map[string]string {
	return map[string]string{"app": "api"}
}

// newStableDeployment returns the api Deployment the operator creates when
// deployment/api is absent.
//
// It is a conscious copy of the Deployment in deploy/k3s/manifests/20-api.yaml,
// which is the original and carries the reasoning behind each field (Recreate,
// the three probes, the D8 memory bounds). apply.sh still renders that
// manifest until O2 moves the api Deployment under the operator, so the two
// must agree: a test decodes the manifest and fails on any difference. An
// adopted Deployment is never rebuilt from this shape; adoption changes only
// its ownerReferences and its api image (D31).
func newStableDeployment(namespace, image string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      stableDeploymentName,
			Namespace: namespace,
			Labels:    apiLabels(),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: new(int32(1)),
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: apiLabels()},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: apiLabels()},
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
							HostPort:      8000,
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
