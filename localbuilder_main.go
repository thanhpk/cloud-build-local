// Copyright 2017 Google, Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package main runs the gcb local builder.
package main // import "github.com/thanhpk/cloud-build-local"

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/otiai10/copy"
	"github.com/pborman/uuid"
	"github.com/spf13/afero"
	"github.com/thanhpk/cloud-build-local/build"
	"github.com/thanhpk/cloud-build-local/common"
	"github.com/thanhpk/cloud-build-local/config"
	"github.com/thanhpk/cloud-build-local/gcloud"
	"github.com/thanhpk/cloud-build-local/metadata"
	"github.com/thanhpk/cloud-build-local/runner"
)

const (
	volumeNamePrefix  = "cloudbuild_vol_"
	gcbDockerVersion  = "17.12.0-ce"
	metadataImageName = "gcr.io/cloud-builders/metadata"
)

var (
	configFile     = flag.String("config", "cloudbuild.yaml", "File path of the config file")
	substitutions  = flag.String("substitutions", "", `key=value pairs where the key is already defined in the build request; separate multiple substitutions with a comma, for example: _FOO=bar,_BAZ=baz`)
	dryRun         = flag.Bool("dryrun", true, "Lints the config file and prints but does not run the commands; Local Builder runs the commands only when dryrun is set to false")
	push           = flag.Bool("push", false, "Pushes the images to the registry")
	noSource       = flag.Bool("no-source", false, "Prevents Local Builder from using source for this build")
	writeWorkspace = flag.String("write-workspace", "", "Copies the workspace directory to this host directory")
	help           = flag.Bool("help", false, "Prints the help message")
	versionFlag    = flag.Bool("version", false, "Prints the local builder version")
)

func exitUsage(msg string) {
	log.Fatalf("%s\nUsage: %s --config=cloudbuild.yaml [--substitutions=_FOO=bar] [--dryrun=true/false] [--push=true/false] source", msg, os.Args[0])
}

func main() {
	if strings.Contains(os.Args[0], "container-builder") {
		log.Printf("WARNING: %v is deprecated. Please run `gcloud install cloud-build-local` to install its replacement.", os.Args[0])
	}
	flag.Parse()
	ctx := context.Background()
	args := flag.Args()

	if *help {
		flag.PrintDefaults()
		return
	}
	if *versionFlag {
		log.Printf("Version: %s", version)
		return
	}

	nbSource := 1
	if *noSource {
		nbSource = 0
	}

	if len(args) < nbSource {
		exitUsage("Specify a source")
	} else if len(args) > nbSource {
		if nbSource == 1 {
			exitUsage("There should be only one positional argument. Pass all the flags before the source.")
		} else {
			exitUsage("no-source flag can't be used along with source.")
		}
	}
	source := ""
	if nbSource == 1 {
		source = args[0]
	}

	if *configFile == "" {
		exitUsage("Specify a config file")
	}

	log.SetOutput(os.Stdout)
	if err := run(ctx, source); err != nil {
		log.Fatal(err)
	}
}

func CopyDir(src string) string {
	dir, err := ioutil.TempDir("/tmp", "cloudbuild")
	if err != nil {
		log.Fatal(err)
	}

	if err := copy.Copy(src, dir); err != nil {
		log.Fatalf("COPY FILE ERROR %v", err)
	}

	return dir
}

// run method is used to encapsulate the local builder process, being
// able to return errors to the main function which will then panic. So the
// run function can probably run all the defer functions in any case.
func run(ctx context.Context, source string) error {
	// Create a runner.
	r := &runner.RealRunner{
		DryRun: *dryRun,
	}

	// Channel to tell goroutines to stop.
	// Do not defer the close() because we want this stop to happen before other
	// defer functions.
	stopchan := make(chan struct{})

	// Clean leftovers from a previous build.
	if err := common.Clean(ctx, r); err != nil {
		return fmt.Errorf("Error cleaning: %v", err)
	}

	// Load config file into a build struct.
	buildConfig, err := config.Load(*configFile)
	if err != nil {
		return fmt.Errorf("Error loading config file: %v", err)
	}
	// When the build is run locally, there will be no build ID. Assign a unique value.
	buildConfig.Id = "localbuild_" + uuid.New()

	// Get the ProjectId to feed both the build and the metadata server.
	// This command uses a runner without dryrun to return the real project.

	projectInfo := metadata.ProjectInfo{
		ProjectID:  "subiz-version-4",
		ProjectNum: 457995922934,
	}
	buildConfig.ProjectId = projectInfo.ProjectID
	substMap := make(map[string]string)
	if *substitutions != "" {
		substMap, err = common.ParseSubstitutionsFlag(*substitutions)
		if err != nil {
			return fmt.Errorf("Error parsing substitutions flag: %v", err)
		}
	}
	if err = common.SubstituteAndValidate(buildConfig, substMap); err != nil {
		return fmt.Errorf("Error merging substitutions and validating build: %v", err)
	}

	// Create a volume, a helper container to copy the source, and defer cleaning.
	workspacepath := ""
	if !*dryRun {
		if source != "" {
			// If the source is a directory, only copy the inner content.
			if isDir, err := isDirectory(source); err != nil {
				return fmt.Errorf("Error getting directory: %v", err)
			} else if isDir {
				source = filepath.Clean(source) + "/."
			}
			t := time.Now()
			workspacepath = CopyDir(source)
			fmt.Println("time to copy", time.Since(t))
			defer os.RemoveAll(workspacepath) // clean up
		}
		//if *writeWorkspace != "" {
		//defer vol.Export(ctx, *writeWorkspace)
		//}
	}

	b := build.New(r, *buildConfig, nil /* TokenSource */, stdoutLogger{}, workspacepath, afero.NewOsFs(), true, *push, *dryRun)

	b.Start(ctx)
	<-b.Done

	close(stopchan)

	if b.GetStatus().BuildStatus == build.StatusError {
		return fmt.Errorf("Build finished with ERROR status")
	}

	if *dryRun {
		log.Printf("Warning: this was a dry run; add --dryrun=false if you want to run the build locally.")
	}
	return nil
}

// supplyTokenToMetadata gets gcloud token and supply it to the metadata server.
func supplyTokenToMetadata(ctx context.Context, metadataUpdater metadata.RealUpdater, r runner.Runner, stopchan <-chan struct{}) {
	for {
		tok, err := gcloud.AccessToken(ctx, r)
		if err != nil {
			log.Printf("Error getting gcloud token: %v", err)
			continue
		}
		if err := metadataUpdater.SetToken(tok); err != nil {
			log.Printf("Error updating token in metadata server: %v", err)
			continue
		}
		refresh := common.RefreshDuration(tok.Expiry)
		select {
		case <-time.After(refresh):
			continue
		case <-stopchan:
			return
		}
	}
}

// dockerVersion gets local server and client docker versions.
func dockerVersions(ctx context.Context, r runner.Runner) (string, string, error) {
	cmd := []string{"docker", "version", "--format", "{{.Server.Version}}"}
	var serverb bytes.Buffer
	if err := r.Run(ctx, cmd, nil, &serverb, os.Stderr, ""); err != nil {
		return "", "", err
	}

	cmd = []string{"docker", "version", "--format", "{{.Client.Version}}"}
	var clientb bytes.Buffer
	if err := r.Run(ctx, cmd, nil, &clientb, os.Stderr, ""); err != nil {
		return "", "", err
	}

	return strings.TrimSpace(serverb.String()), strings.TrimSpace(clientb.String()), nil
}

func isDirectory(path string) (bool, error) {
	fileInfo, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	mode := fileInfo.Mode()
	return mode.IsDir(), nil
}

type stdoutLogger struct{}

func (stdoutLogger) MakeWriter(prefix string, _ int, stdout bool) io.Writer {
	if stdout {
		return prefixWriter{os.Stdout, prefix}
	}
	return prefixWriter{os.Stderr, prefix}
}
func (stdoutLogger) Close() error              { return nil }
func (stdoutLogger) WriteMainEntry(msg string) { fmt.Fprintln(os.Stdout, msg) }

type prefixWriter struct {
	w interface {
		WriteString(string) (int, error)
	}
	prefix string
}

func (pw prefixWriter) Write(b []byte) (int, error) {
	var buf bytes.Buffer
	if n, err := buf.Write(b); err != nil {
		return n, err
	}

	scanner := bufio.NewScanner(&buf)
	for scanner.Scan() {
		line := scanner.Text()
		line = fmt.Sprintf("%s: %s\n", pw.prefix, line)
		pw.w.WriteString(line)
	}
	if err := scanner.Err(); err != nil {
		return -1, err
	}
	return len(b), nil
}
