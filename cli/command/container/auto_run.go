package container

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	apiclient "github.com/docker/docker/client"
	"github.com/pkg/errors"

	"github.com/docker/cli/cli"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/command/image"
	"github.com/docker/distribution/reference"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const (
	ociTitleLabel          = "org.opencontainers.image.title"
	ociDescriptionLabel    = "org.opencontainers.image.description"
	ociDocumentationLabel  = "org.opencontainers.image.documentation"
	autoRMLabel            = "com.docker.auto.rm"
	autoPublishLabel       = "com.docker.auto.publish"
	autoPublishAllLabel    = "com.docker.auto.publish-all"
	autoCmdLabel           = "com.docker.auto.cmd"
	autoInteractiveLabel   = "com.docker.auto.interactive"
	autoTTYLabel           = "com.docker.auto.tty"
	autoPIDLabel           = "com.docker.auto.pid"
	autoNetLabel           = "com.docker.auto.net"
	autoNameLabel          = "com.docker.auto.name"
	autoMountLocalDirLabel = "com.docker.auto.mount-local-dir-to"
	autoEnvLabel           = "com.docker.auto.env"
)

type autoRunOptions struct {
	yes       bool
	print     bool
	quiet     bool
	platform  string
	untrusted bool
	pull      string
}

func NewAutoRunCommand(dockerCli command.Cli) *cobra.Command {
	var opts autoRunOptions
	copts := initContainerOptions()

	cmd := &cobra.Command{
		Use:   "auto-run IMAGE",
		Short: "Run a command in a new container with default configuration",
		Args:  cli.RequiresMinArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			copts.Image = args[0]
			return runAutoRun(dockerCli, cmd.Flags(), &opts, copts)
		},
	}

	flags := cmd.Flags()
	flags.SetInterspersed(false)

	flags.BoolVarP(&opts.yes, "yes", "y", false, "Do not ask confirmation before to run")
	flags.BoolVar(&opts.print, "print", false, "Print the command to run the container and exit")
	flags.BoolVarP(&opts.quiet, "quiet", "q", false, "Do not print documentation and command to run")
	flags.StringVar(&opts.pull, "pull", PullImageAlways,
		`Pull image before creating ("`+PullImageAlways+`"|"`+PullImageMissing+`"|"`+PullImageNever+`")`)

	flags.Bool("help", false, "Print usage")

	command.AddPlatformFlag(flags, &opts.platform)
	command.AddTrustVerificationFlags(flags, &opts.untrusted, dockerCli.ContentTrustEnabled())

	return cmd
}

func runAutoRun(dockerCli command.Cli, flags *pflag.FlagSet, opts *autoRunOptions, copts *containerOptions) error {
	var (
		ctx         = context.Background()
		details     = new(strings.Builder)
		cmd         = new(strings.Builder)
		stderr      io.Writer
		out         io.Writer
		trustedRef  reference.Canonical
		namedRef    reference.Named
		inspect     types.ImageInspect
		needConfirm bool
	)

	stderr = dockerCli.Err()
	if opts.quiet {
		out = io.Discard
	} else {
		out = dockerCli.Err()
	}

	setEnvForProxy(dockerCli, copts)

	// magic

	if err := checkImage(ctx, dockerCli, opts, copts, &trustedRef, &namedRef); err != nil {
		return err
	}

	if opts.pull == PullImageAlways {
		if err := pullAndTagImage(ctx, dockerCli, copts.Image, opts.platform, trustedRef, namedRef, stderr); err != nil {
			return err
		}
	}

	if err := inspectImage(ctx, dockerCli, copts.Image, opts.platform, opts.pull == PullImageMissing, trustedRef, namedRef, stderr, &inspect); err != nil {
		return err
	}

	if !opts.quiet {
		printDocHeader(out, copts.Image, inspect.Config.Labels)
	}

	ropts := &runOptions{
		createOptions: createOptions{
			name:      "",
			platform:  opts.platform,
			untrusted: opts.untrusted,
			pull:      PullImageNever, // previously handled
		},
		detach:   false,
		sigProxy: true,
	}

	_, _ = cmd.WriteString(os.Args[0] + " run")

	if len(flags.Args()) > 1 {
		copts.Args = flags.Args()[1:]
	}

	confirm := false
	if err := parseMagicLabels(cmd, details, &confirm, copts, inspect.Config, ropts); err != nil {
		return err
	}
	needConfirm = confirm && !opts.yes

	if !opts.quiet {
		printRunDetails(out, details, inspect.Config.Labels[autoCmdLabel])
	}

	_, _ = cmd.WriteString(" " + copts.Image)
	if len(copts.Args) > 0 {
		_, _ = cmd.WriteString(" " + strings.Join(copts.Args, " "))
	}

	dockerCmd := cmd.String()

	if opts.print {
		_, _ = fmt.Fprintln(dockerCli.Out(), dockerCmd)
		os.Exit(0)
	}

	if !needConfirm && !opts.quiet {
		_, _ = fmt.Fprintln(stderr, "running:", dockerCmd)
		_, _ = fmt.Fprintln(stderr)
	}

	if needConfirm {
		_, _ = fmt.Fprintf(stderr, `
the following command will be executed:
    %s

are you OK to proceed? ([y]/n) `, dockerCmd)
		var response string

		_, err := fmt.Scanln(&response)
		if err != nil && err.Error() != "unexpected newline" {
			return err
		}

		switch strings.ToLower(strings.TrimSpace(response)) {
		case "", "y", "yes":
			_, _ = fmt.Fprintln(stderr)
		default:
			return errors.New("canceled")
		}
	}

	// end magic

	containerConfig, err := parse(flags, copts, dockerCli.ServerInfo().OSType)
	// just in case the parse does not exit
	if err != nil {
		reportError(dockerCli.Err(), "run", err.Error(), true)
		return cli.StatusError{StatusCode: 125}
	}
	if err = validateAPIVersion(containerConfig, dockerCli.Client().ClientVersion()); err != nil {
		reportError(dockerCli.Err(), "run", err.Error(), true)
		return cli.StatusError{StatusCode: 125}
	}

	return runContainer(dockerCli, ropts, copts, containerConfig)
}

func checkImage(ctx context.Context, dockerCli command.Cli, options *autoRunOptions, copts *containerOptions, trustedRef *reference.Canonical, namedRef *reference.Named) error {
	ref, err := reference.ParseAnyReference(copts.Image)
	if err != nil {
		return err
	}
	if named, ok := ref.(reference.Named); ok {
		*namedRef = reference.TagNameOnly(named)

		if taggedRef, ok := (*namedRef).(reference.NamedTagged); ok && !options.untrusted {
			*trustedRef, err = image.TrustedReference(ctx, dockerCli, taggedRef, nil)
			if err != nil {
				return err
			}
			copts.Image = reference.FamiliarString(*trustedRef)
		}
	}

	return nil
}

func pullAndTagImage(ctx context.Context, dockerCli command.Cli, img, platform string, trustedRef reference.Canonical, namedRef reference.Named, stderr io.Writer) error {
	if err := pullImage(ctx, dockerCli, img, platform, stderr); err != nil {
		return err
	}
	if taggedRef, ok := namedRef.(reference.NamedTagged); ok && trustedRef != nil {
		return image.TagTrusted(ctx, dockerCli, trustedRef, taggedRef)
	}
	return nil
}

func inspectImage(ctx context.Context, dockerCli command.Cli, img, platform string, canPull bool, trustedRef reference.Canonical, namedRef reference.Named, stderr io.Writer, inspect *types.ImageInspect) error {
	var err error
	*inspect, _, err = dockerCli.Client().ImageInspectWithRaw(ctx, img)
	if err != nil {
		if apiclient.IsErrNotFound(err) && namedRef != nil && canPull {
			_, _ = fmt.Fprintf(stderr, "Unable to find image '%s' locally\n", reference.FamiliarString(namedRef))
			if err = pullAndTagImage(ctx, dockerCli, img, platform, trustedRef, namedRef, stderr); err != nil {
				return err
			}
			var retryErr error
			*inspect, _, retryErr = dockerCli.Client().ImageInspectWithRaw(ctx, img)
			if retryErr != nil {
				return retryErr
			}
		} else {
			return err
		}
	}
	return nil
}

var (
	wands = map[string]func(labelValue string, copts *containerOptions, config *container.Config, ropts *runOptions) (cmd, details string, confirm bool, err error){
		autoRMLabel: func(labelValue string, copts *containerOptions, _ *container.Config, _ *runOptions) (cmd, details string, confirm bool, err error) {
			if rm, _ := strconv.ParseBool(labelValue); rm {
				copts.autoRemove = true
				cmd = "--rm"
				details = "[--rm] Automatically remove the container when it exits"
			}
			return
		},
		autoPublishLabel: func(labelValue string, copts *containerOptions, _ *container.Config, _ *runOptions) (cmd, details string, confirm bool, err error) {
			for i, p := range strings.Split(labelValue, ",") {
				_ = copts.publish.Set(strings.TrimSpace(p))
				if i > 0 {
					cmd += " "
				}
				cmd += "--publish " + p
			}
			if cmd != "" {
				details = "[" + cmd + "] Publish a container's port(s) to the host"
				confirm = true
			}
			return
		},
		autoPublishAllLabel: func(labelValue string, copts *containerOptions, config *container.Config, _ *runOptions) (cmd, details string, confirm bool, err error) {
			if publishAll, _ := strconv.ParseBool(labelValue); publishAll {
				for port := range config.ExposedPorts {
					_ = copts.publish.Set(port.Port() + ":" + port.Port() + "/" + port.Proto())
				}
				cmd = "--publish-all"
				details = "[--publish-all] Publish all exposed ports to random ports"
				confirm = true
			}
			return
		},
		autoCmdLabel: func(labelValue string, copts *containerOptions, _ *container.Config, _ *runOptions) (cmd, details string, confirm bool, err error) {
			if len(copts.Args) > 0 {
				// arguments on command line override label value
				return
			}
			var args []string
			args, err = parseCommandLine(labelValue)
			if err != nil {
				return
			}
			copts.Args = args
			return
		},
		autoInteractiveLabel: func(labelValue string, copts *containerOptions, _ *container.Config, _ *runOptions) (cmd, details string, confirm bool, err error) {
			if interactive, _ := strconv.ParseBool(labelValue); interactive {
				copts.stdin = true
				cmd = "--interactive"
				details = "[--interactive] Keep STDIN open even if not attached"
			}
			return
		},
		autoTTYLabel: func(labelValue string, copts *containerOptions, _ *container.Config, _ *runOptions) (cmd, details string, confirm bool, err error) {
			if tty, _ := strconv.ParseBool(labelValue); tty {
				copts.tty = true
				cmd = "--tty"
				details = "[--tty] Allocate a pseudo-TTY"
			}
			return
		},
		autoPIDLabel: func(labelValue string, copts *containerOptions, _ *container.Config, _ *runOptions) (cmd, details string, confirm bool, err error) {
			if pidMode := strings.TrimSpace(labelValue); pidMode != "" {
				copts.pidMode = pidMode
				cmd = "--pid " + pidMode
				details = "[--pid " + pidMode + "] PID namespace to use"
				confirm = true
			}
			return
		},
		autoNetLabel: func(labelValue string, copts *containerOptions, _ *container.Config, _ *runOptions) (cmd, details string, confirm bool, err error) {
			if netMode := strings.TrimSpace(labelValue); netMode != "" {
				if err = copts.netMode.Set(netMode); err != nil {
					return
				}
				cmd = "--net " + netMode
				details = "[--net " + netMode + "] Network config in swarm mode"
				confirm = true
			}
			return
		},
		autoNameLabel: func(labelValue string, _ *containerOptions, _ *container.Config, ropts *runOptions) (cmd, details string, confirm bool, err error) {
			if name := strings.TrimSpace(labelValue); name != "" {
				ropts.name = name
				cmd = "--name " + name
				details = "[--name " + name + "] Assign a name to the container"
			}
			return
		},
		autoMountLocalDirLabel: func(labelValue string, copts *containerOptions, config *container.Config, ropts *runOptions) (cmd, details string, confirm bool, err error) {
			if target := strings.TrimSpace(labelValue); target != "" {
				var pwd string
				pwd, err = os.Getwd()
				if err != nil {
					return
				}
				mount := "type=bind,source=" + pwd + ",target=" + target
				if err = copts.mounts.Set(mount); err != nil {
					return
				}
				cmd = "--mount " + mount
				details = "[--mount " + mount + "] Attach a filesystem mount to the container"
				confirm = true
			}
			return
		},
		autoEnvLabel: func(labelValue string, copts *containerOptions, config *container.Config, ropts *runOptions) (cmd, details string, confirm bool, err error) {
			if envs := strings.TrimSpace(labelValue); envs != "" {
				for i, env := range strings.Split(envs, ",") {
					err = copts.env.Set(env + "=" + os.Getenv(env))
					if err != nil {
						return
					}
					if i > 0 {
						cmd += " "
					}
					cmd += "--env " + env
				}
				if cmd != "" {
					details = "[" + cmd + "] Set environment variables"
				}
			}
			return
		},
	}
)

func parseMagicLabels(cmd *strings.Builder, details *strings.Builder, confirm *bool, copts *containerOptions, config *container.Config, ropts *runOptions) error {
	for name, value := range config.Labels {
		if wand, ok := wands[name]; ok {
			c, d, needConfirm, err := wand(value, copts, config, ropts)
			if err != nil {
				return err
			} else {
				if c != "" {
					cmd.WriteString(" " + c)
				}
				if d != "" {
					details.WriteString("  * " + d + "\n")
				}
				if needConfirm {
					*confirm = true
				}
			}
		}
	}

	return nil
}

func printRunDetails(out io.Writer, details *strings.Builder, cmdArgs string) {
	_, _ = fmt.Fprintf(out, `
Auto generated options:

%s
`, details.String())
	if cmdArgs != "" {
		_, _ = fmt.Fprintf(out, "  * [%s] Arguments to pass to the entrypoint\n", cmdArgs)
	}
	_, _ = fmt.Fprintln(out)
}

func printDocHeader(out io.Writer, imageName string, labels map[string]string) {
	_, _ = fmt.Fprintf(out, `

Auto-running %s

`, imageName)

	if ociTitle, ok := labels[ociTitleLabel]; ok {
		_, _ = fmt.Fprint(out, ociTitle)
		if ociDesc, ok := labels[ociDescriptionLabel]; ok {
			_, _ = fmt.Fprintln(out, ":", ociDesc)
		}
	}
	if ociDoc, ok := labels[ociDocumentationLabel]; ok {
		_, _ = fmt.Fprintln(out, "See more at", ociDoc)
	}
	_, _ = fmt.Fprintln(out)
}
