package subcommand

import (
	"fmt"
	"github.com/hashicorp/consul/agent"
	"github.com/hashicorp/consul/sdk/testutil/retry"
	"github.com/mitchellh/cli"
	"github.com/stretchr/testify/require"
	"io/ioutil"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRun_FlagValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		Flags  []string
		ExpErr string
	}{
		{
			Flags:  []string{""},
			ExpErr: "-service-config must be set",
		},
		{
			Flags:  []string{"-service-config=/config.hcl", "-sync-period=notparseable"},
			ExpErr: "-sync-period is invalid: time: invalid duration notparseable",
		},
	}

	for _, c := range cases {
		t.Run(c.ExpErr, func(t *testing.T) {
			ui := cli.NewMockUi()
			cmd := Command{
				UI: ui,
			}
			responseCode := cmd.Run(c.Flags)
			require.Equal(t, 1, responseCode, ui.ErrorWriter.String())
			require.Contains(t, ui.ErrorWriter.String(), c.ExpErr)
		})
	}
}

func TestRun_ServiceConfigFileMissing(t *testing.T) {
	t.Parallel()
	ui := cli.NewMockUi()
	cmd := Command{
		UI: ui,
	}
	responseCode := cmd.Run([]string{"-service-config=/does/not/exist"})
	require.Equal(t, 1, responseCode, ui.ErrorWriter.String())
	require.Contains(t, ui.ErrorWriter.String(), "-service-config file \"/does/not/exist\" not found")
}

func TestRun_ServiceConfigFileInvalid(t *testing.T) {
	t.Parallel()
	tmpDir, err := ioutil.TempDir("", "")
	require.NoError(t, err)
	defer func() { os.RemoveAll(tmpDir) }()

	cases := []struct {
		FileContents string
		ExpErr       string
	}{
		{
			FileContents: "",
			ExpErr:       "expected 2 services to be defined",
		},
		{
			FileContents: "'",
			ExpErr:       "At 1:1: illegal char",
		},
	}
	for _, c := range cases {
		t.Run(c.ExpErr, func(t *testing.T) {
			configFile := filepath.Join(tmpDir, "svc.hcl")
			err = ioutil.WriteFile(configFile, []byte(c.FileContents), 0600)
			require.NoError(t, err)
			defer func() { os.Remove(configFile) }()

			ui := cli.NewMockUi()
			cmd := Command{
				UI: ui,
			}

			responseCode := cmd.Run([]string{"-service-config", configFile})
			require.Equal(t, 1, responseCode, ui.ErrorWriter.String())
			require.Contains(t, ui.ErrorWriter.String(), c.ExpErr)
		})
	}
}

// Test that we register the services.
func TestRun_ServicesRegistration(t *testing.T) {
	t.Parallel()
	tmpDir, err := ioutil.TempDir("", "")
	require.NoError(t, err)
	defer func() { os.RemoveAll(tmpDir) }()

	configFile := filepath.Join(tmpDir, "svc.hcl")
	err = ioutil.WriteFile(configFile, []byte(servicesRegistration), 0600)
	require.NoError(t, err)

	a := agent.NewTestAgent(t, t.Name(), `primary_datacenter = "dc1"`)
	defer a.Shutdown()

	ui := cli.NewMockUi()
	cmd := Command{
		UI:           ui,
		consulClient: a.Client(),
	}

	// Run async because we need to kill it when the test is over.
	exitChan := runCommandAsynchronously(&cmd, []string{
		"-http-addr", a.HTTPAddr(),
		"-service-config", configFile,
		"-sync-period", "100ms",
	})
	defer stopCommand(t, &cmd, exitChan)

	timer := &retry.Timer{Timeout: 1 * time.Second, Wait: 100 * time.Millisecond}
	retry.RunWith(timer, t, func(r *retry.R) {
		svc, _, err := a.Client().Agent().Service("service-id", nil)
		require.NoError(r, err)
		require.Equal(r, 80, svc.Port)

		svcProxy, _, err := a.Client().Agent().Service("service-id-sidecar-proxy", nil)
		require.NoError(r, err)
		require.Equal(r, 2000, svcProxy.Port)
	})
}

// Test that we register services when the Consul agent is down at first.
// In this test we use an http server to mimic Consul and we start it
// after we start the command.
func TestRun_ServicesRegistration_ConsulDown(t *testing.T) {
	t.Parallel()
	tmpDir, err := ioutil.TempDir("", "")
	require.NoError(t, err)
	defer func() { os.RemoveAll(tmpDir) }()

	configFile := filepath.Join(tmpDir, "svc.hcl")
	err = ioutil.WriteFile(configFile, []byte(servicesRegistration), 0600)
	require.NoError(t, err)

	ui := cli.NewMockUi()
	cmd := Command{
		UI: ui,
	}
	randomPort := rand.Int()%10000 + 10000
	// Run async because we need to kill it when the test is over.
	exitChan := runCommandAsynchronously(&cmd, []string{
		"-http-addr", fmt.Sprintf("127.0.0.1:%d", randomPort),
		"-service-config", configFile,
		"-sync-period", "100ms",
	})
	defer stopCommand(t, &cmd, exitChan)

	// Start the HTTP server after 500ms.
	time.Sleep(500 * time.Millisecond)
	type APICall struct {
		Method string
		Path   string
	}
	var consulAPICalls []APICall
	go func() {
		err := http.ListenAndServe(fmt.Sprintf(":%d", randomPort), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Record all the API calls made.
			consulAPICalls = append(consulAPICalls, APICall{
				Method: r.Method,
				Path:   r.URL.Path,
			})

			// Send an empty JSON response with code 200 to all calls.
			fmt.Fprintln(w, "{}")
		}))
		require.NoError(t, err)
	}()

	// We should get API calls to our server within 500ms.
	timer := &retry.Timer{Timeout: 500 * time.Millisecond, Wait: 100 * time.Millisecond}
	retry.RunWith(timer, t, func(r *retry.R) {
		require.Len(r, consulAPICalls, 2)
	})
}

// This function starts the command asynchronously and returns a non-blocking chan.
// When finished, the command will send its exit code to the channel.
// Note that it's the responsibility of the caller to terminate the command by calling stopCommand,
// otherwise it can run forever.
func runCommandAsynchronously(cmd *Command, args []string) chan int {
	exitChan := make(chan int, 1)
	go func() {
		exitChan <- cmd.Run(args)
	}()
	return exitChan
}

func stopCommand(t *testing.T, cmd *Command, exitChan chan int) {
	if len(exitChan) == 0 {
		cmd.interrupt()
	}
	select {
	case c := <-exitChan:
		require.Equal(t, 0, c, string(cmd.UI.(*cli.MockUi).ErrorWriter.Bytes()))
	}
}

const servicesRegistration = `
services {
	id   = "service-id"
	name = "service"
	port = 80
}
services {
	id   = "service-id-sidecar-proxy"
	name = "service-sidecar-proxy"
	port = 2000
    kind = "connect-proxy"
	proxy {
	  destination_service_name = "service"
	  destination_service_id = "service-id"
	  local_service_port = 80
	}
}`
