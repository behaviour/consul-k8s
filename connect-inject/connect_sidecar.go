package connectinject

import (
	corev1 "k8s.io/api/core/v1"
	"strings"
)

func (h *Handler) connectSidecar(pod *corev1.Pod) corev1.Container {
	command := []string{
		"consul-k8s",
		"connect-sidecar",
		"-service-config", "/consul/connect-inject/service.hcl",
	}
	if h.AuthMethod != "" {
		command = append(command, "-token-file=/consul/connect-inject/acl-token")
	}
	if period, ok := pod.Annotations[annotationSyncPeriod]; ok {
		command = append(command, "-sync-period="+strings.TrimSpace(period))
	}

	return corev1.Container{
		Name:  "consul-connect-sidecar",
		Image: h.ImageConsulK8s,
		Env: []corev1.EnvVar{
			{
				Name: "HOST_IP",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.hostIP"},
				},
			},
			// Kubernetes will interpolate HOST_IP when creating this environment
			// variable.
			{
				Name:  "CONSUL_HTTP_ADDR",
				Value: "$(HOST_IP):8500",
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      volumeName,
				MountPath: "/consul/connect-inject",
			},
		},
		Command: command,
	}
}
