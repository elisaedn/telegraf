//go:build mage

package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"slices"
	"strings"

	goutil "github.com/elisasre/mageutil/golang"
	"github.com/magefile/mage/mg"

	//mage:import go
	_ "github.com/elisasre/mageutil/tool/golangcilint"
	//mage:import
	docker "github.com/elisasre/mageutil/docker/target"
	//mage:import
	golang "github.com/elisasre/mageutil/golang/target"
)

const (
	projectURL  = "https://github.com/elisaedn/telegraf"
	internalPkg = "github.com/influxdata/telegraf/internal"
)

var (
	inputPlugins = []string{
		"inputs.netconf",
		"inputs.gnmi",
		"inputs.jti_openconfig_telemetry",
		"inputs.internal",
		"inputs.snmp",
		"inputs.snmp_trap",
		"inputs.netflow",
	}
	outputPlugins = []string{
		"outputs.health",
		"outputs.kafka",
		"outputs.prometheus_client",
	}
	processors = []string{
		"processors.converter",
		"processors.strings",
		"processors.rename",
		"processors.regex",
		"processors.filter",
	}
	aggregators = []string{
		"aggregators.merge",
	}
	serializers = []string{
		"serializers.json",
	}
)

func init() {
	os.Setenv(mg.VerboseEnv, "1")
	os.Setenv("CGO_ENABLED", "0")
	version := os.Getenv("RELEASE_TAG")
	if version == "" {
		version = "0.0.1"
	}

	tags := strings.Join(slices.Concat(inputPlugins, outputPlugins, processors, aggregators, serializers), ",")
	golang.BuildTarget = "./cmd/telegraf"
	golang.ExtraBuildArgs = []string{
		"-trimpath",
		"-ldflags", fmt.Sprintf("-s -w -X %s.Commit=%s -X %s.Version=%s", internalPkg, gitSha(), internalPkg, version),
		"-tags", fmt.Sprintf("custom,%s", tags),
	}
	golang.CrossBuildFn = mg.F(func(ctx context.Context) error {
		for _, matrix := range golang.BuildMatrix {
			binaryPath := "target/bin/telegraf-" + matrix.OS + "-" + matrix.Arch
			args := append([]string{"build", "-o", binaryPath}, append(golang.ExtraBuildArgs, golang.BuildTarget)...)
			info := goutil.BuildInfo{BinPath: binaryPath, GOOS: matrix.OS, GOARCH: matrix.Arch}
			env := map[string]string{
				"GOOS":   matrix.OS,
				"GOARCH": matrix.Arch,
			}
			_, err := goutil.WithSHA(info, goutil.GoWith(ctx, env, args...))
			if err != nil {
				return err
			}

			if err := createArchive(info); err != nil {
				return err
			}
		}
		return nil
	})
	goutil.DefaultTestCmd = func(ctx context.Context, env map[string]string, _ ...string) error {
		args := []string{
			"-tags=unit",
			"-race",
			"./plugins/inputs/netconf/...",
			"./plugins/inputs/gnmi/...",
			"./plugins/inputs/snmp/...",
			"./plugins/inputs/snmp_trap/...",
			"./plugins/outputs/kafka/...",
		}
		if mg.Verbose() {
			args = append([]string{"-v"}, args...)
		}
		args = append([]string{"test"}, args...)
		return goutil.GoWith(ctx, env, args...)
	}
	docker.ImageName = fmt.Sprintf("%s/edn/telegraf", os.Getenv("DOCKER_REGISTRY"))
	docker.ProjectUrl = projectURL
	docker.ProjectAuthors = "EDN"
	docker.Dockerfile = "docker/Dockerfile"
	docker.ProjectAuthors = "EDN"
}

func gitSha() string {
	buildInfo, ok := debug.ReadBuildInfo()
	if !ok {
		return "n/a"
	}

	for _, setting := range buildInfo.Settings {
		if setting.Key == "vcs.revision" {
			revision := setting.Value
			if len(revision) > 7 {
				revision = revision[0:7]
			}
			return revision
		}
	}
	return "n/a"
}

func createArchive(info goutil.BuildInfo) error {
	archive, err := os.Create(strings.TrimSuffix(info.BinPath, ".exe") + ".tar.gz")
	if err != nil {
		return err
	}
	defer archive.Close()

	gw := gzip.NewWriter(archive)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()

	addFile := func(path string) error {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()

		info, err := file.Stat()
		if err != nil {
			return err
		}

		header, err := tar.FileInfoHeader(info, info.Name())
		if err != nil {
			return err
		}

		if err = tw.WriteHeader(header); err != nil {
			return err
		}

		_, err = io.Copy(tw, file)
		if err != nil {
			return err
		}
		return nil
	}

	for _, file := range []string{info.BinPath, info.BinPath + ".sha256"} {
		if err := addFile(file); err != nil {
			return err
		}
	}
	return nil
}
