package util

import (
	"fmt"
	"strings"

	v1 "k8s.io/api/core/v1"
	networking "k8s.io/api/networking/v1beta1"

	"github.com/G-Research/armada/internal/common/util"
	"github.com/G-Research/armada/internal/executor/configuration"
	"github.com/G-Research/armada/pkg/api"
)

func GenerateIngresses(job *api.Job, pod *v1.Pod, ingressConfig *configuration.IngressConfiguration) ([]*v1.Service, []*networking.Ingress) {
	services := []*v1.Service{}
	ingresses := []*networking.Ingress{}

	ingressToGen := CombineIngressService(job.Ingress, job.Services)

	groupedIngressConfigs := groupIngressConfig(ingressToGen)
	for svcType, configs := range groupedIngressConfigs {
		if len(GetServicePorts(configs, &pod.Spec)) > 0 {
			service := CreateService(job, pod, GetServicePorts(configs, &pod.Spec), svcType, useClusterIP(configs))
			services = append(services, service)

			if svcType == Ingress {
				for index, config := range configs {
					if len(GetServicePorts([]*IngressServiceConfig{config}, &pod.Spec)) <= 0 {
						continue
					}
					ingressName := fmt.Sprintf("%s-%s-%d", pod.Name, strings.ToLower(svcType.String()), index)
					ingress := CreateIngress(ingressName, job, pod, service, ingressConfig, config)
					ingresses = append(ingresses, ingress)
				}
			}
		}
	}

	return services, ingresses
}

func groupIngressConfig(configs []*IngressServiceConfig) map[IngressServiceType][]*IngressServiceConfig {
	result := gatherIngressConfig(configs)

	for ingressType, grp := range result {
		result[ingressType] = mergeOnAnnotations(grp)
	}

	return result
}

// gatherIngressConfig takes a list of ingress configs and groups them by IngressServiceType
func gatherIngressConfig(configs []*IngressServiceConfig) map[IngressServiceType][]*IngressServiceConfig {
	result := make(map[IngressServiceType][]*IngressServiceConfig, 10)

	for _, config := range configs {
		result[config.Type] = append(result[config.Type], deepCopy(config))
	}

	return result
}

func mergeOnAnnotations(configs []*IngressServiceConfig) []*IngressServiceConfig {
	result := make([]*IngressServiceConfig, 0, len(configs))

	for _, config := range configs {
		matchFound := false

		for _, existingConfig := range result {
			if util.Equal(config.Annotations, existingConfig.Annotations) {
				existingConfig.Ports = append(existingConfig.Ports, config.Ports...)
				matchFound = true
			}
		}
		if !matchFound {
			result = append(result, deepCopy(config))
		}
	}

	return result
}

func GetServicePorts(svcConfigs []*IngressServiceConfig, podSpec *v1.PodSpec) []v1.ServicePort {
	var servicePorts []v1.ServicePort

	for _, container := range podSpec.Containers {
		ports := container.Ports
		for _, svcConfig := range svcConfigs {
			for _, port := range ports {
				//Don't expose host via service, this will already be handled by kubernetes
				if port.HostPort > 0 {
					continue
				}
				if contains(svcConfig, uint32(port.ContainerPort)) {
					servicePort := v1.ServicePort{
						Name:     fmt.Sprintf("%s-%d", container.Name, port.ContainerPort),
						Port:     port.ContainerPort,
						Protocol: port.Protocol,
					}
					servicePorts = append(servicePorts, servicePort)
				}
			}
		}
	}

	return servicePorts
}

func contains(portConfig *IngressServiceConfig, port uint32) bool {
	for _, p := range portConfig.Ports {
		if p == port {
			return true
		}
	}
	return false
}
