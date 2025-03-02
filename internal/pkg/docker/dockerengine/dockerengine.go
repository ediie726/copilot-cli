// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

// Package dockerengine provides functionality to interact with the Docker server.
package dockerengine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aws/copilot-cli/internal/pkg/exec"
)

// Cmd is the interface implemented by external commands.
type Cmd interface {
	Run(name string, args []string, options ...exec.CmdOption) error
	RunWithContext(ctx context.Context, name string, args []string, opts ...exec.CmdOption) error
}

// Operating systems and architectures supported by docker.
const (
	OSLinux   = "linux"
	OSWindows = "windows"

	ArchAMD64 = "amd64"
	ArchX86   = "x86_64"
	ArchARM   = "arm"
	ArchARM64 = "arm64"
)

const (
	credStoreECRLogin = "ecr-login" // set on `credStore` attribute in docker configuration file
)

// DockerCmdClient represents the docker client to interact with the server via external commands.
type DockerCmdClient struct {
	runner Cmd
	// Override in unit tests.
	buf       *bytes.Buffer
	homePath  string
	lookupEnv func(string) (string, bool)
}

// New returns CmdClient to make requests against the Docker daemon via external commands.
func New(cmd Cmd) DockerCmdClient {
	return DockerCmdClient{
		runner:    cmd,
		homePath:  userHomeDirectory(),
		lookupEnv: os.LookupEnv,
	}
}

// BuildArguments holds the arguments that can be passed while building a container.
type BuildArguments struct {
	URI        string            // Required. Location of ECR Repo. Used to generate image name in conjunction with tag.
	Tags       []string          // Required. List of tags to apply to the image.
	Dockerfile string            // Required. Dockerfile to pass to `docker build` via --file flag.
	Context    string            // Optional. Build context directory to pass to `docker build`.
	Target     string            // Optional. The target build stage to pass to `docker build`.
	CacheFrom  []string          // Optional. Images to consider as cache sources to pass to `docker build`
	Platform   string            // Optional. OS/Arch to pass to `docker build`.
	Args       map[string]string // Optional. Build args to pass via `--build-arg` flags. Equivalent to ARG directives in dockerfile.
	Labels     map[string]string // Required. Set metadata for an image.
}

// RunOptions holds the options for running a Docker container.
type RunOptions struct {
	ImageURI         string            // Required. The image name to run.
	Secrets          map[string]string // Optional. Secrets to pass to the container as environment variables.
	EnvVars          map[string]string // Optional. Environment variables to pass to the container.
	ContainerName    string            // Optional. The name for the container.
	ContainerPorts   map[string]string // Optional. Contains host and container ports.
	Command          []string          // Optional. The command to run in the container.
	ContainerNetwork string            // Optional. Network mode for the container.
}

// GenerateDockerBuildArgs returns command line arguments to be passed to the Docker build command based on the provided BuildArguments.
// Returns an error if no tags are provided for building an image.
func (in *BuildArguments) GenerateDockerBuildArgs(c DockerCmdClient) ([]string, error) {
	// Tags must not be empty to build an docker image.
	if len(in.Tags) == 0 {
		return nil, &errEmptyImageTags{
			uri: in.URI,
		}
	}
	dfDir := in.Context
	// Context wasn't specified use the Dockerfile's directory as context.
	if dfDir == "" {
		dfDir = filepath.Dir(in.Dockerfile)
	}

	args := []string{"build"}

	// Add additional image tags to the docker build call.
	for _, tag := range in.Tags {
		args = append(args, "-t", imageName(in.URI, tag))
	}

	// Add cache from options.
	for _, imageFrom := range in.CacheFrom {
		args = append(args, "--cache-from", imageFrom)
	}

	// Add target option.
	if in.Target != "" {
		args = append(args, "--target", in.Target)
	}

	// Add platform option.
	if in.Platform != "" {
		args = append(args, "--platform", in.Platform)
	}

	// Plain display if we're in a CI environment.
	if ci, _ := c.lookupEnv("CI"); ci == "true" {
		args = append(args, "--progress", "plain")
	}

	// Add the "args:" override section from manifest to the docker build call.
	// Collect the keys in a slice to sort for test stability.
	var keys []string
	for k := range in.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--build-arg", fmt.Sprintf("%s=%s", k, in.Args[k]))
	}

	// Add Labels to docker build call.
	// Collect the keys in a slice to sort for test stability.
	var labelKeys []string
	for k := range in.Labels {
		labelKeys = append(labelKeys, k)
	}
	sort.Strings(labelKeys)
	for _, k := range labelKeys {
		args = append(args, "--label", fmt.Sprintf("%s=%s", k, in.Labels[k]))
	}

	args = append(args, dfDir, "-f", in.Dockerfile)
	return args, nil
}

type dockerConfig struct {
	CredsStore  string            `json:"credsStore,omitempty"`
	CredHelpers map[string]string `json:"credHelpers,omitempty"`
}

// Build will run a `docker build` command for the given ecr repo URI and build arguments.
func (c DockerCmdClient) Build(ctx context.Context, in *BuildArguments, w io.Writer) error {
	args, err := in.GenerateDockerBuildArgs(c)
	if err != nil {
		return fmt.Errorf("generate docker build args: %w", err)
	}
	if err := c.runner.RunWithContext(ctx, "docker", args, exec.Stdout(w), exec.Stderr(w)); err != nil {
		return fmt.Errorf("building image: %w", err)
	}
	return nil
}

// Login will run a `docker login` command against the Service repository URI with the input uri and auth data.
func (c DockerCmdClient) Login(uri, username, password string) error {
	err := c.runner.Run("docker",
		[]string{"login", "-u", username, "--password-stdin", uri},
		exec.Stdin(strings.NewReader(password)))

	if err != nil {
		return fmt.Errorf("authenticate to ECR: %w", err)
	}

	return nil
}

// Push pushes the images with the specified tags and ecr repository URI, and returns the image digest on success.
func (c DockerCmdClient) Push(ctx context.Context, uri string, w io.Writer, tags ...string) (digest string, err error) {
	images := []string{}
	for _, tag := range tags {
		images = append(images, imageName(uri, tag))
	}
	var args []string
	if ci, _ := c.lookupEnv("CI"); ci == "true" {
		args = append(args, "--quiet")
	}

	for _, img := range images {
		if err := c.runner.RunWithContext(ctx, "docker", append([]string{"push", img}, args...), exec.Stdout(w), exec.Stderr(w)); err != nil {
			return "", fmt.Errorf("docker push %s: %w", img, err)
		}
	}
	buf := new(strings.Builder)
	// The container image will have the same digest regardless of the associated tag.
	// Pick the first tag and get the image's digest.
	// For Main container we call  docker inspect --format '{{json (index .RepoDigests 0)}}' uri:latest
	// For Sidecar container images we call docker inspect --format '{{json (index .RepoDigests 0)}}' uri:<sidecarname>-latest
	if err := c.runner.RunWithContext(ctx, "docker", []string{"inspect", "--format", "'{{json (index .RepoDigests 0)}}'", imageName(uri, tags[0])}, exec.Stdout(buf)); err != nil {
		return "", fmt.Errorf("inspect image digest for %s: %w", uri, err)
	}
	repoDigest := strings.Trim(strings.TrimSpace(buf.String()), `"'`) // remove new lines and quotes from output
	parts := strings.SplitAfter(repoDigest, "@")
	if len(parts) != 2 {
		return "", fmt.Errorf("parse the digest from the repo digest '%s'", repoDigest)
	}
	return parts[1], nil
}

func (in *RunOptions) generateRunArguments() []string {
	args := []string{"run"}

	if in.ContainerName != "" {
		args = append(args, "--name", in.ContainerName)
	}

	for hostPort, containerPort := range in.ContainerPorts {
		args = append(args, "--publish", fmt.Sprintf("%s:%s", hostPort, containerPort))
	}

	// Add network option if it's not a "pause" container.
	if !strings.HasPrefix(in.ContainerName, "pause") {
		args = append(args, "--network", fmt.Sprintf("container:%s", in.ContainerNetwork))
	}

	for key, value := range in.Secrets {
		args = append(args, "--env", fmt.Sprintf("%s=%s", key, value))
	}

	for key, value := range in.EnvVars {
		args = append(args, "--env", fmt.Sprintf("%s=%s", key, value))
	}

	args = append(args, in.ImageURI)

	if in.Command != nil && len(in.Command) > 0 {
		args = append(args, in.Command...)
	}
	return args
}

// Run runs a Docker container with the sepcified options.
func (c DockerCmdClient) Run(ctx context.Context, options *RunOptions) error {
	//Execute the Docker run command.
	if err := c.runner.RunWithContext(ctx, "docker", options.generateRunArguments()); err != nil {
		return fmt.Errorf("running container: %w", err)
	}
	return nil
}

// IsContainerRunning checks if a specific Docker container is running.
func (c DockerCmdClient) IsContainerRunning(containerName string) (bool, error) {
	buf := &bytes.Buffer{}
	if err := c.runner.Run("docker", []string{"ps", "-q", "--filter", "name=" + containerName}, exec.Stdout(buf)); err != nil {
		return false, fmt.Errorf("run docker ps: %w", err)
	}

	output := strings.TrimSpace(buf.String())
	return output != "", nil
}

// CheckDockerEngineRunning will run `docker info` command to check if the docker engine is running.
func (c DockerCmdClient) CheckDockerEngineRunning() error {
	if _, err := osexec.LookPath("docker"); err != nil {
		return ErrDockerCommandNotFound
	}
	buf := &bytes.Buffer{}
	err := c.runner.Run("docker", []string{"info", "-f", "'{{json .}}'"}, exec.Stdout(buf))
	if err != nil {
		return fmt.Errorf("get docker info: %w", err)
	}
	// Trim redundant prefix and suffix. For example: '{"ServerErrors":["Cannot connect...}'\n returns
	// {"ServerErrors":["Cannot connect...}
	out := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(buf.String()), "'"), "'")
	type dockerEngineNotRunningMsg struct {
		ServerErrors []string `json:"ServerErrors"`
	}
	var msg dockerEngineNotRunningMsg
	if err := json.Unmarshal([]byte(out), &msg); err != nil {
		return fmt.Errorf("unmarshal docker info message: %w", err)
	}
	if len(msg.ServerErrors) == 0 {
		return nil
	}
	return &ErrDockerDaemonNotResponsive{
		msg: strings.Join(msg.ServerErrors, "\n"),
	}
}

// GetPlatform will run the `docker version` command to get the OS/Arch.
func (c DockerCmdClient) GetPlatform() (os, arch string, err error) {
	if _, err := osexec.LookPath("docker"); err != nil {
		return "", "", ErrDockerCommandNotFound
	}
	buf := &bytes.Buffer{}
	err = c.runner.Run("docker", []string{"version", "-f", "'{{json .Server}}'"}, exec.Stdout(buf))
	if err != nil {
		return "", "", fmt.Errorf("run docker version: %w", err)
	}

	out := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(buf.String()), "'"), "'")
	type dockerServer struct {
		OS   string `json:"Os"`
		Arch string `json:"Arch"`
	}
	var platform dockerServer
	if err := json.Unmarshal([]byte(out), &platform); err != nil {
		return "", "", fmt.Errorf("unmarshal docker platform: %w", err)

	}
	return platform.OS, platform.Arch, nil
}

func imageName(uri, tag string) string {
	return fmt.Sprintf("%s:%s", uri, tag)
}

// IsEcrCredentialHelperEnabled return true if ecr-login is enabled either globally or registry level
func (c DockerCmdClient) IsEcrCredentialHelperEnabled(uri string) bool {
	// Make sure the program is able to obtain the home directory
	splits := strings.Split(uri, "/")
	if c.homePath == "" || len(splits) == 0 {
		return false
	}

	// Look into the default locations
	pathsToTry := []string{filepath.Join(".docker", "config.json"), ".dockercfg"}
	for _, path := range pathsToTry {
		content, err := os.ReadFile(filepath.Join(c.homePath, path))
		if err != nil {
			// if we can't read the file keep going
			continue
		}

		config, err := parseCredFromDockerConfig(content)
		if err != nil {
			continue
		}

		if config.CredsStore == credStoreECRLogin || config.CredHelpers[splits[0]] == credStoreECRLogin {
			return true
		}
	}

	return false
}

// PlatformString returns a specified of the format <os>/<arch>.
func PlatformString(os, arch string) string {
	return fmt.Sprintf("%s/%s", os, arch)
}

func parseCredFromDockerConfig(config []byte) (*dockerConfig, error) {
	/*
			Sample docker config file
		    {
		        "credsStore" : "ecr-login",
		        "credHelpers": {
		            "dummyaccountId.dkr.ecr.region.amazonaws.com": "ecr-login"
		        }
		    }
	*/
	cred := dockerConfig{}
	err := json.Unmarshal(config, &cred)
	if err != nil {
		return nil, err
	}

	return &cred, nil
}

func userHomeDirectory() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	return home
}

type errEmptyImageTags struct {
	uri string
}

func (e *errEmptyImageTags) Error() string {
	return fmt.Sprintf("tags to reference an image should not be empty for building and pushing into the ECR repository %s", e.uri)
}
