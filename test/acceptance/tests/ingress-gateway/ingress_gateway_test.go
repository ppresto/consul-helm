package connect

import (
	"fmt"
	"testing"

	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/hashicorp/consul-helm/test/acceptance/framework"
	"github.com/hashicorp/consul-helm/test/acceptance/helpers"
	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/consul/sdk/testutil/retry"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Test that ingress gateways work in a default installation.
func TestIngressGatewayDefault(t *testing.T) {
	env := suite.Environment()

	// Initially we start without ingress gateways enabled because we need
	// to create their config entry first.
	helmValues := map[string]string{
		"connectInject.enabled": "true",
	}

	releaseName := helpers.RandomName()
	consulCluster := framework.NewHelmCluster(t, helmValues, env.DefaultContext(t), suite.Config(), releaseName)

	consulCluster.Create(t)

	t.Log("creating server")
	createServer(t, suite.Config(), env.DefaultContext(t).KubectlOptions())

	// We use a "bounce" pod so that we can make calls to the ingress gateway
	// via kubectl exec without needing a route into the cluster from the test machine.
	t.Log("creating bounce pod")
	createBouncePod(t, suite.Config(), env.DefaultContext(t).KubectlOptions())

	// With the cluster up, we can create our ingress-gateway config entry.
	t.Log("creating config entry")
	consulClient := consulCluster.SetupConsulClient(t, false)

	// Create config entry
	created, _, err := consulClient.ConfigEntries().Set(&api.IngressGatewayConfigEntry{
		Kind: api.IngressGateway,
		Name: "ingress-gateway",
		Listeners: []api.IngressListener{
			{
				Port:     8080,
				Protocol: "tcp",
				Services: []api.IngressService{
					{
						Name: "static-server",
					},
				},
			},
		},
	}, nil)
	require.NoError(t, err)
	require.Equal(t, true, created, "config entry failed")

	// Now we can upgrade the cluster and enable ingress gateways.
	t.Log("upgrading helm release with ingress gateways enabled")
	consulCluster.Upgrade(t, map[string]string{
		"ingressGateways.enabled":              "1",
		"ingressGateways.gateways[0].name":     "ingress-gateway",
		"ingressGateways.gateways[0].replicas": "1",
	})

	// With the ingress gateway up, we test that we can make a call to it
	// via the bounce pod. It should route to the static-server pod.
	t.Log("trying calls to ingress gateway")
	k8sClient := env.DefaultContext(t).KubernetesClient(t)
	k8sOptions := env.DefaultContext(t).KubectlOptions()
	checkConnection(t, releaseName, k8sOptions, k8sClient, true)
}

// Test that ingress gateways work with ACLs and TLS enabled.
func TestIngressGatewaySecure(t *testing.T) {
	env := suite.Environment()

	// Initially we start without ingress gateways enabled because we need
	// to create their config entry first.
	helmValues := map[string]string{
		"connectInject.enabled":        "true",
		"global.acls.manageSystemACLs": "true",
		"global.tls.enabled":           "true",
	}

	releaseName := helpers.RandomName()
	consulCluster := framework.NewHelmCluster(t, helmValues, env.DefaultContext(t), suite.Config(), releaseName)

	consulCluster.Create(t)

	t.Log("creating server")
	createServer(t, suite.Config(), env.DefaultContext(t).KubectlOptions())

	// We use a "bounce" pod so that we can make calls to the ingress gateway
	// via kubectl exec without needing a route into the cluster from the test machine.
	t.Log("creating bounce pod")
	createBouncePod(t, suite.Config(), env.DefaultContext(t).KubectlOptions())

	// With the cluster up, we can create our ingress-gateway config entry.
	t.Log("creating config entry")
	consulClient := consulCluster.SetupConsulClient(t, true)

	// Create config entry.
	created, _, err := consulClient.ConfigEntries().Set(&api.IngressGatewayConfigEntry{
		Kind: api.IngressGateway,
		Name: "ingress-gateway",
		Listeners: []api.IngressListener{
			{
				Port:     8080,
				Protocol: "tcp",
				Services: []api.IngressService{
					{
						Name: "static-server",
					},
				},
			},
		},
	}, nil)
	require.NoError(t, err)
	require.Equal(t, true, created, "config entry failed")

	// Now we can upgrade the cluster and enable ingress gateways.
	t.Log("upgrading helm release with ingress gateways enabled")
	consulCluster.Upgrade(t, map[string]string{
		"ingressGateways.enabled":              "1",
		"ingressGateways.gateways[0].name":     "ingress-gateway",
		"ingressGateways.gateways[0].replicas": "1",
	})
	k8sClient := env.DefaultContext(t).KubernetesClient(t)
	k8sOptions := env.DefaultContext(t).KubectlOptions()

	// With the ingress gateway up, we test that we can make a call to it
	// via the bounce pod. It should route to the static-server pod but should
	// fail because there is no intention set.
	t.Log("trying call that should fail to ingress gateway")
	checkConnection(t, releaseName, k8sOptions, k8sClient, false)

	t.Log("creating ingress-gateway => static-server intention")
	_, _, err = consulClient.Connect().IntentionCreate(&api.Intention{
		SourceName:      "ingress-gateway",
		DestinationName: "static-server",
		Action:          api.IntentionActionAllow,
	}, nil)
	require.NoError(t, err)

	t.Log("trying call that should succeed to ingress gateway")
	checkConnection(t, releaseName, k8sOptions, k8sClient, true)
}

// checkConnection checks if bounce pod can talk to static-server.
// If expectSuccess is true, it will expect connection to succeed,
// otherwise it will expect failure due to intentions.
func checkConnection(t *testing.T, releaseName string, options *k8s.KubectlOptions, client kubernetes.Interface, expectSuccess bool) {
	pods, err := client.CoreV1().Pods(options.Namespace).List(metav1.ListOptions{LabelSelector: "app=bounce"})
	require.NoError(t, err)
	require.Len(t, pods.Items, 1)
	retry.Run(t, func(r *retry.R) {
		output, err := helpers.RunKubectlAndGetOutputE(t, options, "exec", pods.Items[0].Name, "--", "curl", "-vvvsSs", "-H", "Host: static-server.ingress.consul", fmt.Sprintf("http://%s-consul-ingress-gateway:8080/", releaseName))
		if expectSuccess {
			require.NoError(r, err)
			require.Contains(r, output, "hello world")
		} else {
			require.Error(r, err)
			require.Contains(r, output, "curl: (52) Empty reply from server")
		}
	})
}

func createServer(t *testing.T, cfg *framework.TestConfig, options *k8s.KubectlOptions) {
	helpers.KubectlApply(t, options, "fixtures/static-server.yaml")

	helpers.Cleanup(t, cfg.NoCleanupOnFailure, func() {
		helpers.KubectlDelete(t, options, "fixtures/static-server.yaml")
	})

	helpers.RunKubectl(t, options, "wait", "--for=condition=available", "deploy/static-server")
}

func createBouncePod(t *testing.T, cfg *framework.TestConfig, options *k8s.KubectlOptions) {
	helpers.KubectlApply(t, options, "fixtures/bounce.yaml")

	helpers.Cleanup(t, cfg.NoCleanupOnFailure, func() {
		helpers.KubectlDelete(t, options, "fixtures/bounce.yaml")
	})

	helpers.RunKubectl(t, options, "wait", "--for=condition=available", "deploy/bounce")
}
