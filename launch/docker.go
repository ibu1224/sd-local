package launch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
)

type docker struct {
	volume            string
	habVolume         string
	setupImage        string
	setupImageVersion string
	useSudo           bool
	commands          []*exec.Cmd
	mutex             *sync.Mutex
	flagVerbose       bool
}

var _ runner = (*docker)(nil)
var execCommand = exec.Command

const (
	// ArtifactsDir is default artifact directory name
	ArtifactsDir = "sd-artifacts"
	// LogFile is default logfile name for build log
	LogFile = "builds.log"
	// The definition of "ScmHost" and "OrgRepo" is in "PipelineFromID" of "screwdriver/screwdriver_local.go"
	scmHost = "screwdriver.cd"
	orgRepo = "sd-local/local-build"
)

func newDocker(setupImage, setupImageVer string, useSudo bool, flagVerbose bool) runner {
	return &docker{
		volume:            "SD_LAUNCH_BIN",
		habVolume:         "SD_LAUNCH_HAB",
		setupImage:        setupImage,
		setupImageVersion: setupImageVer,
		useSudo:           useSudo,
		commands:          make([]*exec.Cmd, 0, 10),
		mutex:             &sync.Mutex{},
		flagVerbose:       flagVerbose,
	}
}

func (d *docker) setupBin() error {
	err := d.execDockerCommand("volume", "create", "--name", d.volume)
	if err != nil {
		return fmt.Errorf("failed to create docker volume: %v", err)
	}

	err = d.execDockerCommand("volume", "create", "--name", d.habVolume)
	if err != nil {
		return fmt.Errorf("failed to create docker hab volume: %v", err)
	}

	mount := fmt.Sprintf("%s:/opt/sd/", d.volume)
	habMount := fmt.Sprintf("%s:/hab", d.habVolume)
	image := fmt.Sprintf("%s:%s", d.setupImage, d.setupImageVersion)
	err = d.execDockerCommand("pull", image)
	if err != nil {
		return fmt.Errorf("failed to pull launcher image: %v", err)
	}

	err = d.execDockerCommand("container", "run", "--rm", "-v", mount, "-v", habMount, image, "--entrypoint", "/bin/echo set up bin")
	if err != nil {
		return fmt.Errorf("failed to prepare build scripts: %v", err)
	}

	return nil
}

func (d *docker) runBuild(buildEntry buildEntry) error {
	environment := buildEntry.Environment[0]

	srcDir := buildEntry.SrcPath
	hostArtDir := buildEntry.ArtifactsPath
	containerArtDir := environment["SD_ARTIFACTS_DIR"]
	buildImage := buildEntry.Image
	logfilePath := filepath.Join(containerArtDir, LogFile)

	srcVol := fmt.Sprintf("%s/:/sd/workspace/src/%s/%s", srcDir, scmHost, orgRepo)
	artVol := fmt.Sprintf("%s/:%s", hostArtDir, containerArtDir)
	binVol := fmt.Sprintf("%s:%s", d.volume, "/opt/sd")
	habVol := fmt.Sprintf("%s:%s", d.habVolume, "/opt/sd/hab")
	configJSON, err := json.Marshal(buildEntry)
	if err != nil {
		return err
	}

	logrus.Infof("Pulling docker image from %s...", buildImage)
	err = d.execDockerCommand("pull", buildImage)
	if err != nil {
		return fmt.Errorf("failed to pull user image %v", err)
	}

	dockerCommandArgs := []string{"container", "run"}
	dockerCommandOptions := []string{"--rm", "-v", srcVol, "-v", artVol, "-v", binVol, "-v", habVol, buildImage, "/opt/sd/local_run.sh", string(configJSON), buildEntry.JobName, environment["SD_API_URL"], environment["SD_STORE_URL"], logfilePath}

	if buildEntry.MemoryLimit != "" {
		dockerCommandOptions = append([]string{fmt.Sprintf("-m%s", buildEntry.MemoryLimit)}, dockerCommandOptions...)
	}

	if buildEntry.UsePrivileged {
		dockerCommandOptions = append([]string{"--privileged"}, dockerCommandOptions...)
	}

	err = d.execDockerCommand(append(dockerCommandArgs, dockerCommandOptions...)...)
	if err != nil {
		return fmt.Errorf("failed to run build container: %v", err)
	}

	return nil
}

func (d *docker) execDockerCommand(args ...string) error {
	commands := append([]string{"docker"}, args...)
	if d.useSudo {
		commands = append([]string{"sudo"}, commands...)
	}
	cmd := execCommand(commands[0], commands[1:]...)
	if d.flagVerbose {
		logrus.Infof("$ %s", strings.Join(commands, " "))
		cmd.Stdout = logrus.StandardLogger().WriterLevel(logrus.InfoLevel)
	}
	cmd.Stderr = logrus.StandardLogger().WriterLevel(logrus.ErrorLevel)
	d.commands = append(d.commands, cmd)
	buf := bytes.NewBuffer(nil)
	cmd.Stderr = buf
	err := cmd.Run()
	if err != nil {
		io.Copy(os.Stderr, buf)
		return err
	}
	return nil
}

func (d *docker) kill(sig os.Signal) {
	killedCmds := make([]*exec.Cmd, 0, 10)

	for _, v := range d.commands {
		var err error
		d.mutex.Lock()
		if v.ProcessState != nil {
			continue
		}
		d.mutex.Unlock()

		if d.useSudo {
			cmd := execCommand("sudo", "kill", fmt.Sprintf("-%v", signum(sig)), strconv.Itoa(v.Process.Pid))
			err = cmd.Run()
		} else {
			err = v.Process.Signal(sig)
		}

		if err != nil {
			logrus.Warn(fmt.Errorf("failed to stop process: %v", err))
		} else {
			killedCmds = append(killedCmds, v)
		}
	}

	err := d.waitForProcess(killedCmds)
	if err != nil {
		logrus.Warn(err)
	}
}

func (d *docker) clean() {
	err := d.execDockerCommand("volume", "rm", "--force", d.volume)

	if err != nil {
		logrus.Warn(fmt.Errorf("failed to remove volume: %v", err))
	}

	err = d.execDockerCommand("volume", "rm", "--force", d.habVolume)

	if err != nil {
		logrus.Warn(fmt.Errorf("failed to remove hab volume: %v", err))
	}
}

func (d *docker) waitForProcess(cmds []*exec.Cmd) error {
	// Reducing this value will make the test faster.
	// However, be sure to specify a time when you can sufficiently confirm that the process is dead.
	t := time.NewTicker(1 * time.Second)
	const retryMax = 9
	retryCnt := 0
	for {
		select {
		case <-t.C:

			retryCnt++
			finish := true

			for _, v := range cmds {
				d.mutex.Lock()
				if v.ProcessState == nil {
					finish = false
				}
				d.mutex.Unlock()
			}
			if finish {
				return nil
			}

			if retryCnt > retryMax {
				return fmt.Errorf("waited %d seconds and could not confirm that the process was dead", retryMax+1)
			}
		}
	}
}

func signum(sig os.Signal) int {
	const numSig = 65

	switch sig := sig.(type) {
	case syscall.Signal:
		i := int(sig)
		if i < 0 || i >= numSig {
			return -1
		}
		return i
	default:
		return -1
	}
}
