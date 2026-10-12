package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/serverapi"
)

// flash_build.go: `benchpod flash --build/--repo/--ref/--artifact` flashes a build uploaded to
// embeddedci.com instead of a local --file. Over embeddedci.com the server runs OpenOCD through
// the pod (POST /api/benchpod/devices/{id}/flash); on the network or USB the CLI downloads the file
// and runs the usual local OpenOCD flash. The server picks the artifact (newest matching build,
// .elf before .hex before .bin), so every client flashes the same file for the same selector.

// buildLookupTimeout bounds resolving and downloading the build (images are a few MB at most).
const buildLookupTimeout = 2 * time.Minute

// fromBuild says the flash names an uploaded build rather than a --file.
func (f *flashFlags) fromBuild() bool {
	for _, v := range []string{f.build, f.repo, f.ref, f.artifact} {
		if strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

func (f *flashFlags) buildSelector() serverapi.BuildSelector {
	return serverapi.BuildSelector{Build: f.build, Repo: f.repo, Ref: f.ref, Artifact: f.artifact}
}

// resolveBuild asks embeddedci.com which file the selector names. With download it also fetches
// the file into a temp dir and points f.file at it; the returned cleanup removes it.
func (f *flashFlags) resolveBuild(g *globalFlags, download bool) (*serverapi.BuildArtifact, func(), error) {
	noop := func() {}
	ctx, cancel := context.WithTimeout(context.Background(), buildLookupTimeout)
	defer cancel()
	api := serverapi.New(cloudServerURL())
	cred, err := cloudCredential(ctx, api)
	if err != nil {
		return nil, noop, fmt.Errorf("flash: %w", err)
	}
	art, err := api.ResolveBuildArtifact(ctx, cred, f.buildSelector())
	if err != nil {
		return nil, noop, fmt.Errorf("flash: find the uploaded build: %w", err)
	}
	if !download {
		log.Printf("flash: %s", art.Describe())
		return art, noop, nil
	}
	dir, err := os.MkdirTemp("", "benchpod-build-")
	if err != nil {
		return nil, noop, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, filepath.Base(art.Name))
	out, err := os.Create(path)
	if err != nil {
		cleanup()
		return nil, noop, err
	}
	err = api.DownloadBuildArtifact(ctx, cred, art, out)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		cleanup()
		return nil, noop, fmt.Errorf("flash: download %s: %w", art.Name, err)
	}
	log.Printf("flash: downloaded %s", art.Describe())
	f.file = path
	return art, cleanup, nil
}

// flashOnServer has embeddedci.com flash art through the pod with its own OpenOCD.
func (f *flashFlags) flashOnServer(g *globalFlags, spec ConnSpec, art *serverapi.BuildArtifact, swclk, swdio int) error {
	var localOnly []string
	for name, set := range map[string]bool{
		"--no-verify": f.noVerify, "--no-reset": f.noReset, "--keep-reset-init": f.keepResetInit,
		"--openocd": strings.TrimSpace(f.openocdBin) != "", "-c/--command": len(f.extraConfigs) > 0,
		"--openocd-arg": len(f.extraArgs) > 0,
	} {
		if set {
			localOnly = append(localOnly, name)
		}
	}
	sort.Strings(localOnly)
	if len(localOnly) > 0 {
		return fmt.Errorf("%s only apply to the local OpenOCD; embeddedci.com always verifies and resets. Drop them, or flash from a direct connection with --server-side=false",
			strings.Join(localOnly, ", "))
	}
	if strings.TrimSpace(f.target) == "" {
		return fmt.Errorf("--target is required (an OpenOCD config such as target/stm32f4x.cfg), or set swd_target in the pod's wiring profile")
	}
	if spec.IsSerial() {
		return fmt.Errorf("--server-side needs the pod's network address or --connection embeddedci:<pod>, not a USB connection")
	}
	ctx, cancel := context.WithTimeout(context.Background(), g.effectiveTimeout(flashTimeout)+time.Minute)
	defer cancel()
	defer installSignalHandler(ctx, cancel)()
	api := serverapi.New(cloudServerURL())
	cred, err := cloudCredential(ctx, api)
	if err != nil {
		return fmt.Errorf("flash: %w", err)
	}
	deviceID, err := cloudDeviceID(ctx, api, cred, spec)
	if err != nil {
		return fmt.Errorf("flash: %w", err)
	}
	if deviceID == "" {
		return fmt.Errorf("flash: the pod is not registered on the signed-in embeddedci.com account; flash it here with --server-side=false")
	}
	req := serverapi.FlashRequest{
		JobID: art.JobID, PackID: art.PackID, ArtifactName: art.Name, Target: f.target,
		LoadAddr: f.loadAddr, SWCLK: swclk, SWDIO: swdio, NReset: f.nreset, Efuse: f.targetPower,
	}
	if f.noConnectUnderReset {
		off := false
		req.ConnectUnderReset = &off
	}
	fmt.Fprintln(os.Stderr, "flash: embeddedci.com is flashing the target (OpenOCD runs on the server)")
	resp, err := api.FlashDevice(ctx, cred, deviceID, req)
	if err != nil {
		return fmt.Errorf("flash: %w", err)
	}
	if resp.Output != "" {
		fmt.Fprint(os.Stdout, resp.Output)
		if !strings.HasSuffix(resp.Output, "\n") {
			fmt.Fprintln(os.Stdout)
		}
	}
	if !resp.OK {
		if resp.TargetUnreachable {
			f.hasNRST = true // the server does not say; assume the pod has its reset pin (rev3+)
			printTargetUnreachableHint(f)
		}
		return fmt.Errorf("flash: %s", resp.Error)
	}
	fmt.Fprintln(os.Stderr, "flash: success")
	return nil
}
