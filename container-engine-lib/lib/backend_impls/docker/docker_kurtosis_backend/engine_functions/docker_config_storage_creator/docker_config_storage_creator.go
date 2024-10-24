package docker_config_storage_creator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/docker/docker/api/types/registry"
	"github.com/kurtosis-tech/kurtosis/container-engine-lib/lib/backend_impls/docker/docker_manager"
	"github.com/kurtosis-tech/stacktrace"
	"github.com/sirupsen/logrus"
)

const (
	// We use this image and version because we already are using this in other projects so there is a high probability
	// that the image is in the local machine's cache
	creatorContainerImage = "alpine:3.17"
	creatorContainerName  = "kurtosis-docker-config-storage-creator"

	shBinaryFilepath = "/bin/sh"
	shCmdFlag        = "-c"
	printfCmdName    = "printf"

	creationSuccessExitCode = 0

	creationCmdMaxRetries     = 2
	creationCmdDelayInRetries = 200 * time.Millisecond

	configFilePath = "config.json"

	sleepSeconds = 1800
)

func CreateDockerConfigStorage(
	ctx context.Context,
	targetNetworkId string,
	volumeName string,
	storageDirPath string,
	dockerManager *docker_manager.DockerManager,
) error {
	entrypointArgs := []string{
		shBinaryFilepath,
		shCmdFlag,
		fmt.Sprintf("sleep %v", sleepSeconds),
	}

	volumeMounts := map[string]string{
		volumeName: storageDirPath,
	}

	createAndStartArgs := docker_manager.NewCreateAndStartContainerArgsBuilder(
		creatorContainerImage,
		creatorContainerName,
		targetNetworkId,
	).WithEntrypointArgs(
		entrypointArgs,
	).WithVolumeMounts(
		volumeMounts,
	).Build()

	containerId, _, err := dockerManager.CreateAndStartContainer(ctx, createAndStartArgs)
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred starting the Docker Config Storage Creator container with these args '%+v'", createAndStartArgs)
	}
	//The killing step has to be executed always in the success and also in the failed case
	defer func() {
		if err = dockerManager.RemoveContainer(context.Background(), containerId); err != nil {
			logrus.Errorf(
				"Launching the Docker Config Creator container with container ID '%v' didn't complete successfully so we "+
					"tried to remove the container we started, but doing so exited with an error:\n%v",
				containerId,
				err)
			logrus.Errorf("ACTION REQUIRED: You'll need to manually remove the container with ID '%v'!!!!!!", containerId)
		}
	}()

	if err := storeConfigInVolume(
		ctx,
		dockerManager,
		containerId,
		creationCmdMaxRetries,
		creationCmdDelayInRetries,
		storageDirPath,
	); err != nil {
		return stacktrace.Propagate(err, "An error occurred creating  Docker config storage in volume.")
	}

	return nil
}

// GetGitHubAuthToken Returns empty string if no token found in [githubAuthTokenFile] or [githubAuthTokenFile] doesn't exist
//func GetGitHubAuthToken() string {
//	tokenBytes, err := os.ReadFile(path.Join(consts.GitHubAuthStorageDirPath, configFilePath))
//	if err != nil {
//		return ""
//	}
//	return string(tokenBytes)
//}

func storeConfigInVolume(
	ctx context.Context,
	dockerManager *docker_manager.DockerManager,
	containerId string,
	maxRetries uint,
	timeBetweenRetries time.Duration,
	storageDirPath string,
) error {
	// Get all the registries from the Docker config
	registries, err := docker_manager.GetAllRegistriesFromDockerConfig()
	if err != nil {
		return stacktrace.NewError("An error occurred getting all registries from Docker config: %v", err)
	}

	cfg := struct {
		Auths map[string]registry.AuthConfig `json:"auths"`
	}{
		Auths: make(map[string]registry.AuthConfig),
	}

	// Add the auths for each registry
	for _, registry := range registries {
		creds, err := docker_manager.GetAuthFromDockerConfig(registry)
		if err != nil {
			return stacktrace.NewError("An error occurred getting auth for registry '%v' from Docker config: %v", registry, err)
		}
		cfg.Auths[registry] = *creds
	}

	b, err := json.Marshal(cfg)
	if err != nil {
		return stacktrace.NewError("An error occurred marshalling the Docker config into JSON: %v", err)
	}

	// Write the config.json to the volume
	commandStr := fmt.Sprintf(
		"%v '%v' > %v",
		printfCmdName,
		string(b),
		fmt.Sprintf("%s/%s", storageDirPath, configFilePath),
	)

	execCmd := []string{
		shBinaryFilepath,
		shCmdFlag,
		commandStr,
	}
	for i := uint(0); i < maxRetries; i++ {
		outputBuffer := &bytes.Buffer{}
		exitCode, err := dockerManager.RunExecCommand(ctx, containerId, execCmd, outputBuffer)
		if err == nil {
			if exitCode == creationSuccessExitCode {
				logrus.Debugf("The Docker config file was successfully added into the volume.")
				return nil
			}
			logrus.Debugf(
				"Docker config storage creation command '%v' returned without a Docker error, but exited with non-%v exit code '%v' and logs:\n%v",
				commandStr,
				creationSuccessExitCode,
				exitCode,
				outputBuffer.String(),
			)
		} else {
			logrus.Debugf(
				"Docker config storage creation command '%v' experienced a Docker error:\n%v",
				commandStr,
				err,
			)
		}

		// Tiny optimization to not sleep if we're not going to run the loop again
		if i < maxRetries {
			time.Sleep(timeBetweenRetries)
		}
	}

	return stacktrace.NewError(
		"The Docker config storage creation didn't return success (as measured by the command '%v') even after retrying %v times with %v between retries",
		commandStr,
		maxRetries,
		timeBetweenRetries,
	)
}