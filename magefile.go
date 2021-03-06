// +build mage

// This is a magefile, and is a "makefile for go".
// See https://magefile.org/
package main

import (
	"context"
	"fmt"
	"go/build"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/carolynvs/magex/pkg"
	"github.com/carolynvs/magex/shx"
	"github.com/carolynvs/magex/xplat"
	"github.com/magefile/mage/mg"
	"github.com/magefile/mage/sh"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

// Default target to run when none is specified
// If not set, running mage will list available targets
// var Default = Build

const (
	registryContainer = "registry"
	mixinsURL         = "https://cdn.porter.sh/mixins/"
)

// Ensure Mage is installed and on the PATH.
func EnsureMage() error {
	return pkg.EnsureMage("")
}

// ConfigureAgent sets up an Azure DevOps agent with Mage and ensures
// that GOPATH/bin is in PATH.
func ConfigureAgent() error {
	err := EnsureMage()
	if err != nil {
		return err
	}

	// Instruct Azure DevOps to add GOPATH/bin to PATH
	gobin := xplat.FilePathJoin(xplat.GOPATH(), "bin")
	err = os.MkdirAll(gobin, 0755)
	if err != nil {
		return errors.Wrapf(err, "could not mkdir -p %s", gobin)
	}
	fmt.Printf("##vso[task.prependpath]%s\n", gobin)
	return nil
}

// Install mixins used by tests and example bundles, if not already installed
func GetMixins() error {
	mixinTag := os.Getenv("MIXIN_TAG")
	if mixinTag == "" {
		mixinTag = "canary"
	}

	mixins := []string{"helm", "arm", "terraform", "kubernetes"}
	var errG errgroup.Group
	for _, mixin := range mixins {
		mixinDir := filepath.Join("bin/mixins/", mixin)
		if _, err := os.Stat(mixinDir); err == nil {
			log.Println("Mixin already installed into bin:", mixin)
			continue
		}

		mixin := mixin
		errG.Go(func() error {
			log.Println("Installing mixin:", mixin)
			mixinURL := mixinsURL + mixin
			_, _, err := porter("mixin", "install", mixin, "--version", mixinTag, "--url", mixinURL).Run()
			return err
		})
	}

	return errG.Wait()
}

// Run a porter command from the bin
func porter(args ...string) sh.PreparedCommand {
	porterPath := filepath.Join("bin", "porter")
	p := sh.Command(porterPath, args...)

	porterHome, _ := filepath.Abs("bin")
	p.Cmd.Env = []string{"PORTER_HOME=" + porterHome}

	return p
}

// Run end-to-end (e2e) tests
func TestE2E() error {
	mg.Deps(StarlDockerRegistry)
	defer StopDockerRegistry()

	// Only do verbose output of tests when called with `mage -v TestE2E`
	v := ""
	if mg.Verbose() {
		v = "-v"
	}

	return sh.RunV("go", shx.CollapseArgs("test", "-tags", "e2e", v, "./tests/e2e/...")...)
}

// Copy the cross-compiled binaries from xbuild into bin.
func UseXBuildBinaries() error {
	pwd, _ := os.Getwd()
	goos := build.Default.GOOS
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}

	copies := map[string]string{
		"bin/latest/porter-$GOOS-amd64$EXT":           "bin/porter$EXT",
		"bin/latest/porter-linux-amd64":               "bin/runtimes/porter-runtime",
		"bin/mixins/exec/latest/exec-$GOOS-amd64$EXT": "bin/mixins/exec/exec$EXT",
		"bin/mixins/exec/latest/exec-linux-amd64":     "bin/mixins/exec/runtimes/exec-runtime",
	}

	r := strings.NewReplacer("$GOOS", goos, "$EXT", ext, "$PWD", pwd)
	for src, dest := range copies {
		src = r.Replace(src)
		dest = r.Replace(dest)
		log.Printf("Copying %s to %s", src, dest)

		destDir := filepath.Dir(dest)
		os.MkdirAll(destDir, 0755)

		err := sh.Copy(dest, src)
		if err != nil {
			return err
		}
	}

	return SetBinExecutable()
}

// Run `chmod +x -R bin`.
func SetBinExecutable() error {
	err := chmodRecursive("bin", 0755)
	return errors.Wrap(err, "could not set +x on the test bin")
}

func chmodRecursive(name string, mode os.FileMode) error {
	return filepath.Walk(name, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		log.Println("chmod +x ", path)
		return os.Chmod(path, mode)
	})
}

// Ensure the docker daemon is started and ready to accept connections.
func StartDocker() error {
	switch runtime.GOOS {
	case "windows":
		err := shx.RunS("powershell", "-c", "Get-Process 'Docker Desktop'")
		if err != nil {
			fmt.Println("Starting Docker Desktop")
			cmd := sh.Command(`C:\Program Files\Docker\Docker\Docker Desktop.exe`)
			err := cmd.Cmd.Start()
			if err != nil {
				return errors.Wrapf(err, "could not start Docker Desktop")
			}
		}
	}

	ready, err := isDockerReady()
	if err != nil {
		return err
	}

	if ready {
		return nil
	}

	fmt.Println("Waiting for the docker service to be ready")
	cxt, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	for {
		select {
		case <-cxt.Done():
			return errors.New("a timeout was reached waiting for the docker service to become unavailable")
		default:
			// Wait and check again
			// Writing a dot on a single line so the CI logs show our progress, instead of a bunch of dots at the end
			fmt.Println(".")
			time.Sleep(time.Second)

			if ready, _ := isDockerReady(); ready {
				fmt.Println("Docker service is ready!")
				return nil
			}
		}
	}
}

func isDockerReady() (bool, error) {
	err := shx.RunS("docker", "ps")
	if !sh.CmdRan(err) {
		return false, errors.Wrap(err, "could not run docker")
	}

	return err == nil, nil
}

// Start a Docker registry to use with the tests.
func StarlDockerRegistry() error {
	mg.Deps(StartDocker)
	if isContainerRunning(registryContainer) {
		return nil
	}

	err := removeContainer(registryContainer)
	if err != nil {
		return err
	}

	fmt.Println("Starting local docker registry")
	return shx.RunE("docker", "run", "-d", "-p", "5000:5000", "--name", registryContainer, "registry:2")
}

// Stop the Docker registry used by the tests.
func StopDockerRegistry() error {
	if containerExists(registryContainer) {
		fmt.Println("Stopping local docker registry")
		return removeContainer(registryContainer)
	}
	return nil
}

func isContainerRunning(name string) bool {
	out, _ := shx.OutputS("docker", "container", "inspect", "-f", "{{.State.Running}}", name)
	running, _ := strconv.ParseBool(out)
	return running
}

func containerExists(name string) bool {
	err := shx.RunS("docker", "inspect", name)
	return err == nil
}

func removeContainer(name string) error {
	stderr, err := shx.OutputE("docker", "rm", "-f", name)
	// Gracefully handle the container already being gone
	if err != nil && !strings.Contains(stderr, "No such container") {
		return err
	}
	return nil
}
