package docker

import (
	"context"
	"fmt"
	"os"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"

	"replicate.ai/cli/pkg/console"
)

type closeFunc func() error

// Run runs a Docker container from imageName with cmd
func Run(dockerClient *client.Client, imageName string, cmd []string) error {
	// use same name for both container and image
	containerName := imageName

	// Options for creating container
	config := &container.Config{
		Image: imageName,
		Cmd:   cmd,
	}
	// Options for starting container (port bindings, volume bindings, etc)
	hostConfig := &container.HostConfig{
		AutoRemove: false, // TODO: probably true
	}

	ctx, cancelFun := context.WithCancel(context.Background())
	defer cancelFun()

	createResponse, err := dockerClient.ContainerCreate(ctx, config, hostConfig, nil, nil, containerName)
	if err != nil {
		return err
	}
	for _, warning := range createResponse.Warnings {
		console.Warn("WARNING: %s", warning)
	}

	statusChan := waitUntilExit(ctx, dockerClient, createResponse.ID)

	if err := dockerClient.ContainerStart(ctx, createResponse.ID, types.ContainerStartOptions{}); err != nil {
		return err
	}

	// TODO: detached mode
	var errChan chan error
	close, err := connectToLogs(ctx, dockerClient, &errChan, createResponse.ID)
	if err != nil {
		return err
	}
	defer close()

	if errChan != nil {
		if err := <-errChan; err != nil {
			return err
		}
	}

	status := <-statusChan
	if status != 0 {
		return fmt.Errorf("Command exited with non-zero status code: %v", status)
	}

	return nil
}

// Based on waitExitOrRemoved in github.com/docker/cli cli/command/container/utils.go
func waitUntilExit(ctx context.Context, dockerClient *client.Client, containerID string) <-chan int {
	// TODO check for API version >=1.30

	resultChan, errChan := dockerClient.ContainerWait(ctx, containerID, container.WaitConditionNextExit)

	statusChan := make(chan int)
	go func() {
		select {
		case result := <-resultChan:
			if result.Error != nil {
				console.Error("Error waiting for container: %v", result.Error.Message)
				statusChan <- 125
			} else {
				statusChan <- int(result.StatusCode)
			}
		case err := <-errChan:
			console.Error("error waiting for container: %v", err)
			statusChan <- 125
		}
	}()

	return statusChan
}

// Based on containerAttach in github.com/docker/cli cli/command/container/run.go, but using logs instead of attach
func connectToLogs(ctx context.Context, dockerClient *client.Client, errChan *chan error, containerID string) (closeFunc, error) {
	response, err := dockerClient.ContainerLogs(ctx, containerID, types.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	})
	if err != nil {
		return nil, err
	}

	ch := make(chan error, 1)
	*errChan = ch

	go func() {
		ch <- func() error {
			_, errCopy := stdcopy.StdCopy(os.Stdout, os.Stderr, response)
			return errCopy
		}()
	}()

	return response.Close, nil
}
