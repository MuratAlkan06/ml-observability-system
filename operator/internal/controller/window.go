package controller

import (
	servingv1alpha1 "github.com/MuratAlkan06/ml-observability-system/operator/api/v1alpha1"
)

// canaryRequested reports whether spec asks for a canary window: a canary
// image tag and at least one canary replica, the shape a human's host-side
// patch gives it (docs/PLAN.md D32).
func canaryRequested(spec servingv1alpha1.ServingDeploymentSpec) bool {
	return spec.CanaryImageTag != "" && spec.CanaryReplicas >= 1
}
