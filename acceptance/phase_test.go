package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	ih "github.com/buildpacks/imgutil/testhelpers"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/registry"

	"github.com/buildpacks/lifecycle/auth"
	"github.com/buildpacks/lifecycle/platform"
	h "github.com/buildpacks/lifecycle/testhelpers"
)

type PhaseTest struct {
	containerBinaryDir     string // The path to copy lifecycle binaries to before building the test image.
	containerBinaryPath    string // The path to invoke when running the test container.
	phaseName              string // The phase name, such as detect, analyze, restore, build, export, or create.
	testImageDockerContext string // The directory containing the Dockerfile for the test image.
	testImageRef           string // The test image to run.
	targetDaemon           *targetDaemon
	targetRegistry         *targetRegistry // The target registry to use. Remove by passing `withoutRegistry` to the constructor.
}

type targetDaemon struct {
	os       string
	arch     string
	fixtures *daemonImageFixtures
}

type daemonImageFixtures struct {
	AppImage   string
	CacheImage string
	RunImage   string
}

type targetRegistry struct {
	authConfig      string
	dockerConfigDir string
	network         string
	fixtures        *regImageFixtures
	registry        *ih.DockerRegistry
}

type regImageFixtures struct {
	InaccessibleImage      string
	ReadOnlyAppImage       string
	ReadOnlyCacheImage     string
	ReadOnlyRunImage       string
	ReadWriteAppImage      string
	ReadWriteCacheImage    string
	ReadWriteOtherAppImage string
	SomeAppImage           string
	SomeCacheImage         string
}

func NewPhaseTest(t *testing.T, phaseName, testImageDockerContext string, phaseOp ...func(*PhaseTest)) *PhaseTest {
	phaseTest := &PhaseTest{
		containerBinaryDir:     filepath.Join(testImageDockerContext, "container", "cnb", "lifecycle"),
		containerBinaryPath:    "/cnb/lifecycle/" + phaseName,
		phaseName:              phaseName,
		targetDaemon:           newTargetDaemon(t),
		targetRegistry:         &targetRegistry{},
		testImageDockerContext: testImageDockerContext,
		testImageRef:           "lifecycle/acceptance/" + phaseName,
	}

	for _, op := range phaseOp {
		op(phaseTest)
	}

	return phaseTest
}

func newTargetDaemon(t *testing.T) *targetDaemon {
	info, err := h.DockerCli(t).Info(context.TODO())
	h.AssertNil(t, err)

	arch := info.Architecture
	if arch == "x86_64" {
		arch = "amd64"
	}
	if arch == "aarch64" {
		arch = "arm64"
	}

	return &targetDaemon{
		os:       info.OSType,
		arch:     arch,
		fixtures: nil,
	}
}

func (p *PhaseTest) RegRepoName(repoName string) string {
	return p.targetRegistry.registry.RepoName(repoName)
}

func (p *PhaseTest) Start(t *testing.T, phaseOp ...func(*testing.T, *PhaseTest)) {
	p.targetDaemon.createFixtures(t)

	if p.targetRegistry != nil {
		p.targetRegistry.start(t)
		containerDockerConfigDir := filepath.Join(p.testImageDockerContext, "container", "docker-config")
		h.AssertNil(t, os.RemoveAll(containerDockerConfigDir))
		h.AssertNil(t, os.MkdirAll(containerDockerConfigDir, 0755))
		h.RecursiveCopy(t, p.targetRegistry.dockerConfigDir, containerDockerConfigDir)
	}

	for _, op := range phaseOp {
		op(t, p)
	}

	h.MakeAndCopyLifecycle(t, p.targetDaemon.os, p.targetDaemon.arch, p.containerBinaryDir)
	h.DockerBuild(t, p.testImageRef, p.testImageDockerContext, h.WithArgs("-f", filepath.Join(p.testImageDockerContext, dockerfileName)))
}

func (p *PhaseTest) Stop(t *testing.T) {
	p.targetDaemon.removeFixtures(t)

	if p.targetRegistry != nil {
		p.targetRegistry.stop(t)
		// remove images that were built locally before being pushed to test registry
		cleanupDaemonFixtures(t, *p.targetRegistry.fixtures)
	}

	h.DockerImageRemove(t, p.testImageRef)
}

func (d *targetDaemon) createFixtures(t *testing.T) {
	if d.fixtures != nil {
		return
	}

	var fixtures daemonImageFixtures

	appMeta := minifyMetadata(t, filepath.Join("testdata", "app_image_metadata.json"), platform.LayersMetadata{})
	cacheMeta := minifyMetadata(t, filepath.Join("testdata", "cache_image_metadata.json"), platform.CacheMetadata{})

	fixtures.AppImage = "some-app-image-" + h.RandString(10)
	cmd := exec.Command(
		"docker",
		"build",
		"-t", fixtures.AppImage,
		"--build-arg", "fromImage="+containerBaseImage,
		"--build-arg", "metadata="+appMeta,
		filepath.Join("testdata", "app-image"),
	) // #nosec G204
	h.Run(t, cmd)

	fixtures.CacheImage = "some-cache-image-" + h.RandString(10)
	cmd = exec.Command(
		"docker",
		"build",
		"-t", fixtures.CacheImage,
		"--build-arg", "fromImage="+containerBaseImage,
		"--build-arg", "metadata="+cacheMeta,
		filepath.Join("testdata", "cache-image"),
	) // #nosec G204
	h.Run(t, cmd)

	fixtures.RunImage = "some-run-image-" + h.RandString(10)
	cmd = exec.Command(
		"docker",
		"build",
		"-t", fixtures.RunImage,
		"--build-arg", "fromImage="+containerBaseImage,
		filepath.Join("testdata", "cache-image"),
	) // #nosec G204
	h.Run(t, cmd)

	d.fixtures = &fixtures
}

func (d *targetDaemon) removeFixtures(t *testing.T) {
	cleanupDaemonFixtures(t, *d.fixtures)
}

func (r *targetRegistry) start(t *testing.T) {
	var err error

	r.dockerConfigDir, err = os.MkdirTemp("", "test.docker.config.dir")
	h.AssertNil(t, err)

	sharedRegHandler := registry.New(registry.Logger(log.New(io.Discard, "", log.Lshortfile)))
	r.registry = ih.NewDockerRegistry(
		ih.WithAuth(r.dockerConfigDir),
		ih.WithSharedHandler(sharedRegHandler),
		ih.WithImagePrivileges(),
	)
	r.registry.Start(t)

	// if registry is listening on localhost, use host networking to allow containers to reach it
	r.network = "default"
	if r.registry.Host == "localhost" {
		r.network = "host"
	}

	// Save auth config
	os.Setenv("DOCKER_CONFIG", r.dockerConfigDir)
	r.authConfig, err = auth.BuildEnvVar(authn.DefaultKeychain, r.registry.RepoName("some-repo")) // repo name doesn't matter
	h.AssertNil(t, err)

	r.createFixtures(t)
}

func (r *targetRegistry) createFixtures(t *testing.T) {
	var fixtures regImageFixtures

	appMeta := minifyMetadata(t, filepath.Join("testdata", "app_image_metadata.json"), platform.LayersMetadata{})
	cacheMeta := minifyMetadata(t, filepath.Join("testdata", "cache_image_metadata.json"), platform.CacheMetadata{})

	// With Permissions

	fixtures.InaccessibleImage = r.registry.SetInaccessible("inaccessible-image")

	someReadOnlyAppName := "some-read-only-app-image-" + h.RandString(10)
	fixtures.ReadOnlyAppImage = buildRegistryImage(
		t,
		someReadOnlyAppName,
		filepath.Join("testdata", "app-image"),
		r.registry,
		"--build-arg", "fromImage="+containerBaseImage,
		"--build-arg", "metadata="+appMeta,
	)
	r.registry.SetReadOnly(someReadOnlyAppName)

	someReadOnlyCacheImage := "some-read-only-cache-image-" + h.RandString(10)
	fixtures.ReadOnlyCacheImage = buildRegistryImage(
		t,
		someReadOnlyCacheImage,
		filepath.Join("testdata", "cache-image"),
		r.registry,
		"--build-arg", "fromImage="+containerBaseImage,
		"--build-arg", "metadata="+cacheMeta,
	)
	r.registry.SetReadOnly(someReadOnlyCacheImage)

	someRunImageName := "some-read-only-run-image-" + h.RandString(10)
	buildRegistryImage(
		t,
		someRunImageName,
		filepath.Join("testdata", "cache-image"),
		r.registry,
		"--build-arg", "fromImage="+containerBaseImageFull,
	)
	fixtures.ReadOnlyRunImage = r.registry.SetReadOnly(someRunImageName)

	readWriteAppName := "some-read-write-app-image-" + h.RandString(10)
	fixtures.ReadWriteAppImage = buildRegistryImage(
		t,
		readWriteAppName,
		filepath.Join("testdata", "app-image"),
		r.registry,
		"--build-arg", "fromImage="+containerBaseImage,
		"--build-arg", "metadata="+appMeta,
	)
	r.registry.SetReadWrite(readWriteAppName)

	someReadWriteCacheName := "some-read-write-cache-image-" + h.RandString(10)
	fixtures.ReadWriteCacheImage = buildRegistryImage(
		t,
		someReadWriteCacheName,
		filepath.Join("testdata", "cache-image"),
		r.registry,
		"--build-arg", "fromImage="+containerBaseImage,
		"--build-arg", "metadata="+cacheMeta,
	)
	r.registry.SetReadWrite(someReadWriteCacheName)

	readWriteOtherAppName := "some-other-read-write-app-image-" + h.RandString(10)
	fixtures.ReadWriteOtherAppImage = buildRegistryImage(
		t,
		readWriteOtherAppName,
		filepath.Join("testdata", "app-image"),
		r.registry,
		"--build-arg", "fromImage="+containerBaseImage,
		"--build-arg", "metadata="+appMeta,
	)
	r.registry.SetReadWrite(readWriteOtherAppName)

	// Without Permissions

	fixtures.SomeAppImage = buildRegistryImage(
		t,
		"some-app-image-"+h.RandString(10),
		filepath.Join("testdata", "app-image"),
		r.registry,
		"--build-arg", "fromImage="+containerBaseImage,
		"--build-arg", "metadata="+appMeta,
	)

	fixtures.SomeCacheImage = buildRegistryImage(
		t,
		"some-cache-image-"+h.RandString(10),
		filepath.Join("testdata", "cache-image"),
		r.registry,
		"--build-arg", "fromImage="+containerBaseImage,
		"--build-arg", "metadata="+cacheMeta,
	)

	r.fixtures = &fixtures
}

func (r *targetRegistry) stop(t *testing.T) {
	r.registry.Stop(t)
	os.Unsetenv("DOCKER_CONFIG")
	os.RemoveAll(r.dockerConfigDir)
}

func buildRegistryImage(t *testing.T, repoName, context string, registry *ih.DockerRegistry, buildArgs ...string) string {
	// Build image
	regRepoName := registry.RepoName(repoName)
	h.DockerBuild(t, regRepoName, context, h.WithArgs(buildArgs...))

	// Push image
	h.AssertNil(t, h.PushImage(h.DockerCli(t), regRepoName, registry.EncodedLabeledAuth()))

	// Return registry repo name
	return regRepoName
}

func cleanupDaemonFixtures(t *testing.T, fixtures interface{}) {
	v := reflect.ValueOf(fixtures)

	for i := 0; i < v.NumField(); i++ {
		imageName := fmt.Sprintf("%v", v.Field(i).Interface())
		if imageName == "" {
			continue
		}
		if strings.Contains(imageName, "inaccessible") {
			continue
		}
		h.DockerImageRemove(t, imageName)
	}
}

func minifyMetadata(t *testing.T, path string, metadataStruct interface{}) string {
	metadata, err := os.ReadFile(path)
	h.AssertNil(t, err)

	// Unmarshal and marshal to strip unnecessary whitespace
	h.AssertNil(t, json.Unmarshal(metadata, &metadataStruct))
	flatMetadata, err := json.Marshal(metadataStruct)
	h.AssertNil(t, err)

	return string(flatMetadata)
}

func withoutDaemonFixtures(phaseTest *PhaseTest) {
	phaseTest.targetDaemon.fixtures = &daemonImageFixtures{}
}

func withoutRegistry(phaseTest *PhaseTest) {
	phaseTest.targetRegistry = nil
}
