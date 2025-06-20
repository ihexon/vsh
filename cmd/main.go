//go:build darwin

package main

import (
	"context"
	"fmt"
	"linuxvm/pkg/define"
	"linuxvm/pkg/filesystem"
	"linuxvm/pkg/libkrun"
	"linuxvm/pkg/network"
	"linuxvm/pkg/server"
	"linuxvm/pkg/system"
	"linuxvm/pkg/vmconfig"
	"os"
	"path/filepath"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v3"
	"golang.org/x/sync/errgroup"
)

func main() {
	app := cli.Command{
		Name:        os.Args[0],
		Usage:       "run a linux shell in 1 second",
		UsageText:   os.Args[0] + " [command] [flags]",
		Description: "run a linux shell in 1 second",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "rootfs",
				Usage:    "rootfs path, e.g. /var/lib/libkrun/rootfs/alpine-3.15.0",
				Required: true,
			},
			&cli.Int8Flag{
				Name:  "cpus",
				Usage: "given how many cpu cores",
				Value: 1,
			},
			&cli.Int32Flag{
				Name:  "memory",
				Usage: "set memory in MB",
				Value: 512,
			},
			&cli.StringSliceFlag{
				Name:  "envs",
				Usage: "set envs for cmdline, e.g. --envs=FOO=bar --envs=BAZ=qux",
			},
			&cli.StringSliceFlag{
				Name:  "data-disk",
				Usage: "set data disk path, the disk will be map into /dev/vdX",
			},
			&cli.StringSliceFlag{
				Name:  "mount",
				Usage: "mount host dir to guest dir",
			},
		},
		Action: CreateVM,
	}

	app.DisableSliceFlagSeparator = true

	if err := app.Run(context.Background(), os.Args); err != nil {
		logrus.Fatal(err)
	}
}

func CreateVM(ctx context.Context, command *cli.Command) error {
	err := system.Rlimit()
	if err != nil {
		logrus.Infof("failed to set rlimit: %v", err)
		return err
	}

	vmc := vmconfig.VMConfig{
		MemoryInMB: command.Int32("memory"),
		Cpus:       command.Int8("cpus"),
		RootFS:     command.String("rootfs"),
		DataDisk:   command.StringSlice("data-disk"),
		Mounts:     filesystem.CmdLineMountToMounts(command.StringSlice("mount")),
	}

	tmpdir, err := os.MkdirTemp("", "gvproxy")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %v", err)
	}

	vmc.GVproxyEndpoint = fmt.Sprintf("unix://%s/gvproxy-control.sock", tmpdir)
	vmc.NetworkStackBackend = fmt.Sprintf("unixgram://%s/vfkit-network-backend.sock", tmpdir)

	cmdline := vmconfig.Cmdline{
		Workspace:     "/",
		TargetBin:     "/bootstrap-arm64",
		TargetBinArgs: append([]string{command.Args().First()}, command.Args().Tail()...),
		Env:           command.StringSlice("envs"),
	}

	logrus.Infof("set memory to: %v", vmc.MemoryInMB)
	logrus.Infof("set cpus to: %v", vmc.Cpus)
	logrus.Infof("set rootfs to: %v", vmc.RootFS)
	logrus.Infof("set gvproxy control: %q", vmc.GVproxyEndpoint)
	logrus.Infof("set network backend: %q", vmc.NetworkStackBackend)
	logrus.Infof("set envs: %v", cmdline.Env)
	logrus.Infof("set data disk: %v", vmc.DataDisk)
	logrus.Infof("set cmdline: %q, %q", cmdline.TargetBin, cmdline.TargetBinArgs)

	if err = system.CopyBootstrapInToRootFS(vmc.RootFS); err != nil {
		return fmt.Errorf("failed to copy dhclient4 to rootfs: %v", err)
	}

	if err = vmc.WriteToJsonFile(filepath.Join(vmc.RootFS, define.VMConfig)); err != nil {
		return fmt.Errorf("failed to write vmconfig to json file: %v", err)
	}

	g, ctx := errgroup.WithContext(ctx)

	// vmc must be a static struct at this point
	g.Go(func() error {
		return network.StartNetworking(ctx, vmc)
	})

	g.Go(func() error {
		return server.IgnServer(ctx, &vmc)
	})

	g.Go(func() error {
		return libkrun.StartVM(ctx, vmc, cmdline)
	})

	return g.Wait()
}
