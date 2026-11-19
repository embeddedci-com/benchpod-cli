package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/ci"
	"github.com/embeddedci-com/benchpod-cli/internal/serverapi"
	"github.com/spf13/cobra"
)

// ── runs on embeddedci.com ──────────────────────────────────────────────────

// runsAPI is the embeddedci.com client and credential for the runs API (a longer HTTP timeout:
// a run's files can be large).
func runsAPI(ctx context.Context) (*serverapi.Client, string, error) {
	api := newServerAPI(cloudServerURL())
	api.HTTP = &http.Client{Timeout: 10 * time.Minute}
	cred, err := cloudCredential(ctx, api)
	if err != nil {
		return nil, "", err
	}
	return api, cred, nil
}

// ciSignedIn says whether this machine has a credential for embeddedci.com.
func ciSignedIn(ctx context.Context) (bool, error) {
	_, _, err := runsAPI(ctx)
	if errors.Is(err, errNoCloudCredential) {
		return false, nil
	}
	return err == nil, err
}

// uploadRunFolder uploads run.json, then the files UploadFiles picks, and records the run's id
// and URL in run.json. A file that fails is a warning on warn; run.json failing is the error.
func uploadRunFolder(ctx context.Context, dir string, warn io.Writer) (*ci.Uploaded, error) {
	api, cred, err := runsAPI(ctx)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "run.json")
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(body) > ci.MaxUploadRunJSON {
		return nil, fmt.Errorf("run.json is %d bytes, more than embeddedci.com takes (%d)", len(body), ci.MaxUploadRunJSON)
	}
	run, err := ci.ParseRun(body)
	if err != nil {
		return nil, fmt.Errorf("run.json: %w", err)
	}
	created, err := api.CreateRun(ctx, cred, body)
	if err != nil {
		return nil, fmt.Errorf("upload run.json: %w", err)
	}
	files, skipped := ci.UploadFiles(dir, run)
	for _, s := range skipped {
		fmt.Fprintf(warn, "upload: skipped %s\n", s)
	}
	var total int64
	failed := 0
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			continue
		}
		if len(data) > ci.MaxUploadFile {
			fmt.Fprintf(warn, "upload: skipped %s (%d bytes, more than the %d a file may have)\n", name, len(data), ci.MaxUploadFile)
			continue
		}
		if total+int64(len(data)) > ci.MaxUploadRunFiles {
			fmt.Fprintf(warn, "upload: skipped %s and the rest: the run's files would pass %d bytes\n", name, ci.MaxUploadRunFiles)
			break
		}
		if err := api.UploadRunFile(ctx, cred, created.ID, name, data); err != nil {
			failed++
			fmt.Fprintf(warn, "upload: %s: %v\n", name, err)
			continue
		}
		total += int64(len(data))
	}
	u := &ci.Uploaded{ID: created.ID, URL: created.URL, BuildID: created.BuildID}
	if err := ci.SetUploaded(path, *u); err != nil {
		fmt.Fprintf(warn, "upload: record the upload in run.json: %v\n", err)
	}
	if failed > 0 {
		fmt.Fprintf(warn, "upload: %d of %d files failed; the run is on embeddedci.com without them\n", failed, len(files))
	}
	return u, nil
}

func newCIUploadCmd() *cobra.Command {
	var again bool
	cmd := &cobra.Command{
		Use:   "upload <run folder>",
		Short: "Upload a run folder to embeddedci.com (a run made offline or with --no-upload)",
		Long: "Upload a run folder that `benchpod ci` made without uploading it: run.json, junit.xml,\n" +
			"the logs, the traces its keep policy keeps (failed cases only by default) and the\n" +
			"attachments they point to. The run's id and URL are written into run.json (uploaded).\n" +
			"Needs `benchpod login` or BENCHPOD_API_KEY (with the benchpod:control or jobs:submit scope).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := args[0]
			if st, err := os.Stat(dir); err == nil && !st.IsDir() {
				dir = filepath.Dir(dir) // run.json given
			}
			run, err := ci.ReadRun(dir)
			if err != nil {
				return &ciExit{code: ci.ExitSetup, msg: err.Error()}
			}
			if run.Uploaded != nil && !again {
				return &ciExit{code: ci.ExitSetup, msg: fmt.Sprintf("this run is uploaded already: %s (pass --again to upload it once more)", run.Uploaded.URL)}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			u, err := uploadRunFolder(ctx, dir, cmd.ErrOrStderr())
			if err != nil {
				return &ciExit{code: ci.ExitSetup, msg: "upload: " + err.Error()}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Run URL: %s\n", u.URL)
			return nil
		},
	}
	cmd.Flags().BoolVar(&again, "again", false, "upload a run that is uploaded already (it becomes a second run on embeddedci.com)")
	return cmd
}
