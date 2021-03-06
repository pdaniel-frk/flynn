package client

import (
	"fmt"
	"io"
	"net/url"
	"os"

	"github.com/docker/docker/pkg/resolvconf"
	log "github.com/flynn/flynn/Godeps/_workspace/src/github.com/Sirupsen/logrus"
	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/docker/docker/opts"
	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/docker/docker/pkg/promise"
	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/docker/docker/pkg/signal"
	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/docker/docker/runconfig"
	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/docker/docker/utils"
)

func (cid *cidFile) Close() error {
	cid.file.Close()

	if !cid.written {
		if err := os.Remove(cid.path); err != nil {
			return fmt.Errorf("failed to remove the CID file '%s': %s \n", cid.path, err)
		}
	}

	return nil
}

func (cid *cidFile) Write(id string) error {
	if _, err := cid.file.Write([]byte(id)); err != nil {
		return fmt.Errorf("Failed to write the container ID to the file: %s", err)
	}
	cid.written = true
	return nil
}

func (cli *DockerCli) CmdRun(args ...string) error {
	// FIXME: just use runconfig.Parse already
	cmd := cli.Subcmd("run", "IMAGE [COMMAND] [ARG...]", "Run a command in a new container", true)

	// These are flags not stored in Config/HostConfig
	var (
		flAutoRemove = cmd.Bool([]string{"#rm", "-rm"}, false, "Automatically remove the container when it exits")
		flDetach     = cmd.Bool([]string{"d", "-detach"}, false, "Run container in background and print container ID")
		flSigProxy   = cmd.Bool([]string{"#sig-proxy", "-sig-proxy"}, true, "Proxy received signals to the process")
		flName       = cmd.String([]string{"#name", "-name"}, "", "Assign a name to the container")
		flAttach     *opts.ListOpts

		ErrConflictAttachDetach               = fmt.Errorf("Conflicting options: -a and -d")
		ErrConflictRestartPolicyAndAutoRemove = fmt.Errorf("Conflicting options: --restart and --rm")
		ErrConflictDetachAutoRemove           = fmt.Errorf("Conflicting options: --rm and -d")
	)

	config, hostConfig, cmd, err := runconfig.Parse(cmd, args)
	// just in case the Parse does not exit
	if err != nil {
		utils.ReportError(cmd, err.Error(), true)
	}

	if len(hostConfig.Dns) > 0 {
		// check the DNS settings passed via --dns against
		// localhost regexp to warn if they are trying to
		// set a DNS to a localhost address
		for _, dnsIP := range hostConfig.Dns {
			if resolvconf.IsLocalhost(dnsIP) {
				fmt.Fprintf(cli.err, "WARNING: Localhost DNS setting (--dns=%s) may fail in containers.\n", dnsIP)
				break
			}
		}
	}
	if config.Image == "" {
		cmd.Usage()
		return nil
	}

	if !*flDetach {
		if err := cli.CheckTtyInput(config.AttachStdin, config.Tty); err != nil {
			return err
		}
	} else {
		if fl := cmd.Lookup("-attach"); fl != nil {
			flAttach = fl.Value.(*opts.ListOpts)
			if flAttach.Len() != 0 {
				return ErrConflictAttachDetach
			}
		}
		if *flAutoRemove {
			return ErrConflictDetachAutoRemove
		}

		config.AttachStdin = false
		config.AttachStdout = false
		config.AttachStderr = false
		config.StdinOnce = false
	}

	// Disable flSigProxy when in TTY mode
	sigProxy := *flSigProxy
	if config.Tty {
		sigProxy = false
	}

	createResponse, err := cli.createContainer(config, hostConfig, hostConfig.ContainerIDFile, *flName)
	if err != nil {
		return err
	}
	if sigProxy {
		sigc := cli.forwardAllSignals(createResponse.ID)
		defer signal.StopCatch(sigc)
	}
	var (
		waitDisplayId chan struct{}
		errCh         chan error
	)
	if !config.AttachStdout && !config.AttachStderr {
		// Make this asynchronous to allow the client to write to stdin before having to read the ID
		waitDisplayId = make(chan struct{})
		go func() {
			defer close(waitDisplayId)
			fmt.Fprintf(cli.out, "%s\n", createResponse.ID)
		}()
	}
	if *flAutoRemove && (hostConfig.RestartPolicy.Name == "always" || hostConfig.RestartPolicy.Name == "on-failure") {
		return ErrConflictRestartPolicyAndAutoRemove
	}
	// We need to instantiate the chan because the select needs it. It can
	// be closed but can't be uninitialized.
	hijacked := make(chan io.Closer)
	// Block the return until the chan gets closed
	defer func() {
		log.Debugf("End of CmdRun(), Waiting for hijack to finish.")
		if _, ok := <-hijacked; ok {
			log.Errorf("Hijack did not finish (chan still open)")
		}
	}()
	if config.AttachStdin || config.AttachStdout || config.AttachStderr {
		var (
			out, stderr io.Writer
			in          io.ReadCloser
			v           = url.Values{}
		)
		v.Set("stream", "1")
		if config.AttachStdin {
			v.Set("stdin", "1")
			in = cli.in
		}
		if config.AttachStdout {
			v.Set("stdout", "1")
			out = cli.out
		}
		if config.AttachStderr {
			v.Set("stderr", "1")
			if config.Tty {
				stderr = cli.out
			} else {
				stderr = cli.err
			}
		}
		errCh = promise.Go(func() error {
			return cli.hijack("POST", "/containers/"+createResponse.ID+"/attach?"+v.Encode(), config.Tty, in, out, stderr, hijacked, nil)
		})
	} else {
		close(hijacked)
	}
	// Acknowledge the hijack before starting
	select {
	case closer := <-hijacked:
		// Make sure that the hijack gets closed when returning (results
		// in closing the hijack chan and freeing server's goroutines)
		if closer != nil {
			defer closer.Close()
		}
	case err := <-errCh:
		if err != nil {
			log.Debugf("Error hijack: %s", err)
			return err
		}
	}

	defer func() {
		if *flAutoRemove {
			if _, _, err = readBody(cli.call("DELETE", "/containers/"+createResponse.ID+"?v=1", nil, false)); err != nil {
				log.Errorf("Error deleting container: %s", err)
			}
		}
	}()

	//start the container
	if _, _, err = readBody(cli.call("POST", "/containers/"+createResponse.ID+"/start", nil, false)); err != nil {
		return err
	}

	if (config.AttachStdin || config.AttachStdout || config.AttachStderr) && config.Tty && cli.isTerminalOut {
		if err := cli.monitorTtySize(createResponse.ID, false); err != nil {
			log.Errorf("Error monitoring TTY size: %s", err)
		}
	}

	if errCh != nil {
		if err := <-errCh; err != nil {
			log.Debugf("Error hijack: %s", err)
			return err
		}
	}

	// Detached mode: wait for the id to be displayed and return.
	if !config.AttachStdout && !config.AttachStderr {
		// Detached mode
		<-waitDisplayId
		return nil
	}

	var status int

	// Attached mode
	if *flAutoRemove {
		// Autoremove: wait for the container to finish, retrieve
		// the exit code and remove the container
		if _, _, err := readBody(cli.call("POST", "/containers/"+createResponse.ID+"/wait", nil, false)); err != nil {
			return err
		}
		if _, status, err = getExitCode(cli, createResponse.ID); err != nil {
			return err
		}
	} else {
		// No Autoremove: Simply retrieve the exit code
		if !config.Tty {
			// In non-TTY mode, we can't detach, so we must wait for container exit
			if status, err = waitForExit(cli, createResponse.ID); err != nil {
				return err
			}
		} else {
			// In TTY mode, there is a race: if the process dies too slowly, the state could
			// be updated after the getExitCode call and result in the wrong exit code being reported
			if _, status, err = getExitCode(cli, createResponse.ID); err != nil {
				return err
			}
		}
	}
	if status != 0 {
		return &utils.StatusError{StatusCode: status}
	}
	return nil
}
