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
	ociTitleLabel         = "org.opencontainers.image.title"
	ociDescriptionLabel   = "org.opencontainers.image.description"
	ociDocumentationLabel = "org.opencontainers.image.documentation"
	autoRMLabel           = "com.docker.auto.rm"
	autoPublishLabel      = "com.docker.auto.publish"
	autoPublishAllLabel   = "com.docker.auto.publish-all"
	autoCmdLabel          = "com.docker.auto.cmd"
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
		ctx        = context.Background()
		stderr     = dockerCli.Err()
		trustedRef reference.Canonical
		namedRef   reference.Named
		inspect    types.ImageInspect
		cmd        string
	)

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
		printDocHeader(stderr, copts.Image, inspect.Config.Labels)
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

	if err := parseMagicLabels(stderr, inspect.Config, copts); err != nil {
		return err
	}

	if opts.print {
		_, _ = fmt.Fprintln(dockerCli.Out(), os.Args[0], cmd)
		os.Exit(0)
	}

	if opts.yes && !opts.quiet {
		_, _ = fmt.Fprintln(stderr, "running:", os.Args[0], cmd)
	}

	if !opts.yes {
		_, _ = fmt.Fprintf(stderr, `
the following command will be executed:
    %s %s

are you OK to proceed? ([y]/n) `, os.Args[0], cmd)
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
	wands = map[string]func(labelValue string, config *container.Config, copts *containerOptions) error{
		autoRMLabel: func(labelValue string, _ *container.Config, copts *containerOptions) error {
			if rm, _ := strconv.ParseBool(labelValue); rm {
				copts.autoRemove = true
			}
			return nil
		},
		autoPublishLabel: func(labelValue string, _ *container.Config, copts *containerOptions) error {
			for _, p := range strings.Split(labelValue, ",") {
				_ = copts.publish.Set(strings.TrimSpace(p))
			}
			return nil
		},
		autoPublishAllLabel: func(labelValue string, config *container.Config, copts *containerOptions) error {
			if publishAll, _ := strconv.ParseBool(labelValue); publishAll {
				for port := range config.ExposedPorts {
					_ = copts.publish.Set(port.Port() + ":" + port.Port() + "/" + port.Proto())
				}
			}
			return nil
		},
		autoCmdLabel: func(labelValue string, _ *container.Config, copts *containerOptions) error {
			args, err := parseCommandLine(labelValue)
			if err != nil {
				return err
			}
			copts.Args = args
			return nil
		},
	}
)

func parseMagicLabels(stderr io.Writer, config *container.Config, copts *containerOptions) error {
	_, _ = fmt.Fprintln(stderr, `

👷 auto generated options:`)

	for name, value := range config.Labels {
		if wand, ok := wands[name]; ok {
			if err := wand(value, config, copts); err != nil {
				return err
			}
		}
	}

	return nil
}

func printDocHeader(stderr io.Writer, imageName string, labels map[string]string) {
	_, _ = fmt.Fprintf(stderr, `

🪄 Auto-running %s
`, imageName)

	if ociTitle, ok := labels[ociTitleLabel]; ok {
		_, _ = fmt.Fprint(stderr, ociTitle)
		if ociDesc, ok := labels[ociDescriptionLabel]; ok {
			_, _ = fmt.Fprintln(stderr, ":", ociDesc)
		}
	}
	if ociDoc, ok := labels[ociDocumentationLabel]; ok {
		_, _ = fmt.Fprintln(stderr, "📒 See more at", ociDoc)
	}
	_, _ = fmt.Fprintln(stderr)
}
