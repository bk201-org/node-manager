package integration

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	nodeconfigv1 "github.com/harvester/node-manager/pkg/apis/node.harvesterhci.io/v1beta1"
	"github.com/kevinburke/ssh_config"
	"github.com/melbahja/goph"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	k8smetav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"

	clientset "github.com/harvester/node-manager/pkg/generated/clientset/versioned"
)

type NTPSuite struct {
	suite.Suite
	sshClient      *goph.Client
	clientSet      *clientset.Clientset
	targetNodeName string
}

const defaultNTPServers = "0.opensuse.pool.ntp.org 1.opensuse.pool.ntp.org 2.opensuse.pool.ntp.org 3.opensuse.pool.ntp.org"

// resetTimesyncdFailure clears systemd-timesyncd's "failed" state before a
// config change that will restart it. BeforeTest/body/AfterTest can each
// cause a restart, and back-to-back subtests can trip systemd's default
// unit rate-limit (5 starts per 10s), wedging the service in a permanent
// "start-limit-hit" failed state that a plain restart can't recover from.
// Resetting here clears that counter proactively instead of just spacing
// restarts out and hoping the limit isn't hit.
func (s *NTPSuite) resetTimesyncdFailure() {
	// Best-effort: this is cleanup, not the behavior under test, so a
	// failure here shouldn't fail the test outright, but logging error
	// is worth surfacing.
	if out, err := s.sshClient.Run("sudo systemctl reset-failed systemd-timesyncd"); err != nil {
		s.T().Logf("resetTimesyncdFailure: systemctl reset-failed systemd-timesyncd failed: %v (output: %s)", err, out)
	}
}

func (s *NTPSuite) SetupSuite() {
	sshConfigPath := os.Getenv("CLUSTER_SSH_CONFIG")
	require.NotEmpty(s.T(), sshConfigPath, "CLUSTER_SSH_CONFIG should not be empty")
	privateKey := os.Getenv("CLUSTER_SSH_KEY")
	require.NotEmpty(s.T(), privateKey, "CLUSTER_SSH_KEY should not be empty")
	nodeName := os.Getenv("CLUSTER_NODE")
	require.NotEmpty(s.T(), nodeName, "CLUSTER_NODE should not be empty")
	kubeconfig := os.Getenv("KUBECONFIG")
	require.NotEmpty(s.T(), kubeconfig, "KUBECONFIG should not be empty")
	s.targetNodeName = nodeName

	sshConfig, err := os.ReadFile(sshConfigPath)
	require.NoError(s.T(), err, "Failed to read ssh-config")

	cfg, err := ssh_config.DecodeBytes(sshConfig)
	require.NoError(s.T(), err, "Failed to decode ssh-config")

	targetHost, err := cfg.Get(nodeName, "HostName")
	require.NoError(s.T(), err, "Failed to get HostName from ssh-config")
	require.NotEmpty(s.T(), targetHost, "HostName of %s should not be empty", nodeName)
	targetUser, err := cfg.Get(nodeName, "User")
	require.NoError(s.T(), err, "Failed to get User from ssh-config")
	require.NotEmpty(s.T(), targetUser, "User of %s should not be empty", nodeName)

	auth, err := goph.Key(privateKey, "")
	require.NoError(s.T(), err, "Failed to generate ssh auth key")

	s.sshClient, err = goph.NewUnknown(targetUser, targetHost, auth)
	require.NoError(s.T(), err, "Failed to create ssh connection")

	require.FileExists(s.T(), kubeconfig, "kubeconfig should exist")
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	require.NoError(s.T(), err, "Failed to generate clientset config")

	s.clientSet, err = clientset.NewForConfig(config)
	require.NoError(s.T(), err, "Failed to create clientset")
}

// setup default NTPServers
func (s *NTPSuite) BeforeTest(_, _ string) {
	newNodeConfig := &nodeconfigv1.NodeConfig{
		ObjectMeta: k8smetav1.ObjectMeta{
			Name:      s.targetNodeName,
			Namespace: "harvester-system",
		},
		Spec: nodeconfigv1.NodeConfigSpec{
			NTPConfig: &nodeconfigv1.NTPConfig{
				NTPServers: defaultNTPServers,
			},
		},
	}

	nodeConfigs := s.clientSet.NodeV1beta1().NodeConfigs("harvester-system")
	nodeConfig, err := nodeConfigs.Get(context.TODO(), s.targetNodeName, k8smetav1.GetOptions{})
	s.resetTimesyncdFailure()
	if err != nil {
		_, err = nodeConfigs.Create(context.TODO(), newNodeConfig, k8smetav1.CreateOptions{})
		require.NoError(s.T(), err, "Failed to create NodeConfig")
	} else {
		updateNodeConfig := nodeConfig.DeepCopy()
		updateNodeConfig.Spec.NTPConfig = newNodeConfig.Spec.NTPConfig
		_, err = nodeConfigs.Update(context.TODO(), updateNodeConfig, k8smetav1.UpdateOptions{})
		require.NoError(s.T(), err, "Failed to update NodeConfig")
	}

	require.Eventually(s.T(), func() bool {
		out, _ := s.sshClient.Run("timedatectl show-timesync")
		return strings.Contains(string(out), fmt.Sprintf("SystemNTPServers=%s", defaultNTPServers))
	}, 20*time.Second, 1*time.Second, fmt.Sprintf("NTPServers should be %s", defaultNTPServers))
}

// restore default NTPServers
func (s *NTPSuite) AfterTest(_, _ string) {
	nodeConfigs := s.clientSet.NodeV1beta1().NodeConfigs("harvester-system")

	nodeConfig, err := nodeConfigs.Get(context.TODO(), s.targetNodeName, k8smetav1.GetOptions{})
	require.NoError(s.T(), err, "Failed to get NodeConfig")
	require.NotNil(s.T(), nodeConfig, "NodeConfig should not be nil")

	updateNodeConfig := nodeConfig.DeepCopy()
	updateNodeConfig.Spec.NTPConfig = &nodeconfigv1.NTPConfig{NTPServers: defaultNTPServers}

	s.resetTimesyncdFailure()
	_, err = nodeConfigs.Update(context.TODO(), updateNodeConfig, k8smetav1.UpdateOptions{})
	require.NoError(s.T(), err, "Failed to update NodeConfig")
	require.Eventually(s.T(), func() bool {
		out, _ := s.sshClient.Run("timedatectl show-timesync")
		return strings.Contains(string(out), fmt.Sprintf("SystemNTPServers=%s", defaultNTPServers))
	}, 20*time.Second, 1*time.Second, fmt.Sprintf("NTPServers should be %s", defaultNTPServers))
}

func (s *NTPSuite) TearDownSuite() {
	if s.sshClient != nil {
		s.sshClient.Close() //nolint:errcheck
	}
}

func TestNTPServerConfig(t *testing.T) {
	suite.Run(t, new(NTPSuite))
}

func (s *NTPSuite) TestNTP() {
	nodeConfigs := s.clientSet.NodeV1beta1().NodeConfigs("harvester-system")

	googleNTPServers := "time1.google.com time2.google.com"

	nodeConfig, err := nodeConfigs.Get(context.TODO(), s.targetNodeName, k8smetav1.GetOptions{})
	require.NoError(s.T(), err, "Failed to get NodeConfig")
	require.NotNil(s.T(), nodeConfig, "NodeConfig should not be nil")

	updateNodeConfig := nodeConfig.DeepCopy()
	updateNodeConfig.Spec.NTPConfig = &nodeconfigv1.NTPConfig{NTPServers: googleNTPServers}

	s.resetTimesyncdFailure()
	_, err = nodeConfigs.Update(context.TODO(), updateNodeConfig, k8smetav1.UpdateOptions{})
	require.NoError(s.T(), err, "Failed to create NodeConfig")

	require.Eventually(s.T(), func() bool {
		out, _ := s.sshClient.Run("timedatectl show-timesync")
		return strings.Contains(string(out), fmt.Sprintf("SystemNTPServers=%s", googleNTPServers))
	}, 20*time.Second, 1*time.Second, fmt.Sprintf("NTPServers should be %s", googleNTPServers))
}

// TestNTPDualStack locks in that a space-separated NTPServers value mixing
// hostnames with IPv4 and IPv6 literals (dual-stack) is passed through
// correctly. The input contains a duplicate entry, so the test also
// exercises the controller's dedupe behavior. systemd-timesyncd's NTP=
// contacts entries in the given order, so the configured order must be
// preserved, with only the duplicate dropped.
func (s *NTPSuite) TestNTPDualStack() {
	nodeConfigs := s.clientSet.NodeV1beta1().NodeConfigs("harvester-system")

	dualStackNTPServers := "fd00::1 192.0.2.1 0.opensuse.pool.ntp.org 192.0.2.1 2001:db8::123"
	expectedNTPServers := "fd00::1 192.0.2.1 0.opensuse.pool.ntp.org 2001:db8::123"

	nodeConfig, err := nodeConfigs.Get(context.TODO(), s.targetNodeName, k8smetav1.GetOptions{})
	require.NoError(s.T(), err, "Failed to get NodeConfig")
	require.NotNil(s.T(), nodeConfig, "NodeConfig should not be nil")

	updateNodeConfig := nodeConfig.DeepCopy()
	updateNodeConfig.Spec.NTPConfig = &nodeconfigv1.NTPConfig{NTPServers: dualStackNTPServers}

	s.resetTimesyncdFailure()
	_, err = nodeConfigs.Update(context.TODO(), updateNodeConfig, k8smetav1.UpdateOptions{})
	require.NoError(s.T(), err, "Failed to update NodeConfig")

	require.Eventually(s.T(), func() bool {
		out, _ := s.sshClient.Run("timedatectl show-timesync")
		return strings.Contains(string(out), fmt.Sprintf("SystemNTPServers=%s", expectedNTPServers))
	}, 20*time.Second, 1*time.Second, fmt.Sprintf("NTPServers should be %s", expectedNTPServers))
}
