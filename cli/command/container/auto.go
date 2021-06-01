package container

import (
	"context"
	"errors"
	"fmt"
	"github.com/docker/cli/cli"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/command/image"
	"github.com/docker/cli/opts"
	"github.com/docker/distribution/reference"
	apiclient "github.com/docker/docker/client"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"io"
	"strconv"
	"strings"
)

type autoOptions struct {
	platform string
	untrusted bool
	pull string // always, missing, never
	y bool
}

func NewAutoCommand(dockerCli command.Cli) *cobra.Command {
	var opts autoOptions
	copts := initContainerOptions()

	cmd := &cobra.Command{
		Use: "auto IMAGE",
		Short: "Run a command in a new container with default configuration",
		Args: cli.RequiresMinArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			copts.Image = args[0]
			return runAuto(dockerCli, cmd.Flags(),&opts, copts)
		},
	}

	flags := cmd.Flags()
	flags.SetInterspersed(false)

	flags.StringVar(&opts.pull, "pull", PullImageAlways,
		`Pull image before creating ("`+PullImageAlways+`"|"`+PullImageMissing+`")`)
	flags.BoolVarP(&opts.y, "yes", "y", false, "Do not prompt for confirmation to run")

	flags.Bool("help", false, "Print usage")

	command.AddPlatformFlag(flags, &opts.platform)
	command.AddTrustVerificationFlags(flags, &opts.untrusted, dockerCli.ContentTrustEnabled())

	return cmd
}

func setEnvForProxy(dockerCli command.Cli, copts *containerOptions) {
	proxyConfig := dockerCli.ConfigFile().ParseProxyConfig(dockerCli.Client().DaemonHost(), opts.ConvertKVStringsToMapWithNil(copts.env.GetAll()))
	newEnv := []string{}
	for k, v := range proxyConfig {
		if v == nil {
			newEnv = append(newEnv, k)
		} else {
			newEnv = append(newEnv, fmt.Sprintf("%s=%s", k, *v))
		}
	}
	copts.env = *opts.NewListOptsRef(&newEnv, nil)
}

func checkImage(ctx context.Context, dockerCli command.Cli, options *autoOptions, copts *containerOptions, trustedRef *reference.Canonical, namedRef *reference.Named) error {
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

func runAuto(dockerCli command.Cli, flags *pflag.FlagSet, options *autoOptions, copts *containerOptions) error {
	var (
		ctx = context.Background()
		stderr = dockerCli.Err()
		trustedRef reference.Canonical
		namedRef reference.Named
	)

	setEnvForProxy(dockerCli, copts)

	if err := checkImage(ctx, dockerCli, options, copts, &trustedRef, &namedRef); err != nil {
		return err
	}

	if options.pull == PullImageAlways {
		if err := pullAndTagImage(ctx, dockerCli, copts.Image, options.platform, trustedRef, namedRef, stderr); err != nil {
			return err
		}
	}

	inspect, _, err := dockerCli.Client().ImageInspectWithRaw(ctx, copts.Image)
	if err != nil {
		if apiclient.IsErrNotFound(err) && namedRef != nil && options.pull == PullImageMissing {
			fmt.Fprintf(stderr, "Unable to find image '%s' locally\n", reference.FamiliarString(namedRef))
			if err = pullAndTagImage(ctx, dockerCli, copts.Image, options.platform, trustedRef, namedRef, stderr); err != nil {
				return err
			}
			var retryErr error
			inspect, _, retryErr = dockerCli.Client().ImageInspectWithRaw(ctx, copts.Image)
			if retryErr != nil {
				return retryErr
			}
		} else {
			return err
		}
	}

	const (
		ociTitleLabel = "org.opencontainers.image.title"
		ociDescriptionLabel = "org.opencontainers.image.description"
		ociDocumentationLabel = "org.opencontainers.image.documentation"
		autoRMLabel = "com.docker.auto.rm"
		autoPublishLabel = "com.docker.auto.publish"
		autoPublishAllLabel = "com.docker.auto.publish-all"
		autoCmdLabel = "com.docker.auto.cmd"
	)

	_, _ = fmt.Fprintln(stderr)
	_, _ = fmt.Fprintf(stderr,
		"%s: %s\nSee more at %s\n\n",
		inspect.Config.Labels[ociTitleLabel],
		inspect.Config.Labels[ociDescriptionLabel],
		inspect.Config.Labels[ociDocumentationLabel])

	_, _ = fmt.Fprintln(stderr, "auto-generated options:")

	cmd := []string{"docker run"}

	if rm, ok := inspect.Config.Labels[autoRMLabel]; ok {
		autoRM, _ := strconv.ParseBool(rm)
		copts.autoRemove = autoRM
		if autoRM {
			cmd = append(cmd, "--rm")
			_, _ = fmt.Fprintln(stderr, "- [--rm] Automatically remove the container when it exits")
		}
	}
	if publish, ok := inspect.Config.Labels[autoPublishLabel]; ok {
		var pubcmd []string
		for _, p := range strings.Split(publish, ",") {
			_ = copts.publish.Set(strings.TrimSpace(p))
			pubcmd = append(pubcmd, "--publish", p)
		}
		if len(pubcmd) != 0 {
			pub := strings.Join(pubcmd, " ")
			cmd = append(cmd, pub)
			_, _ = fmt.Fprintf(stderr, "- [%s] Publish a container's port(s) to the host\n", pub)
		}
	}
	if publishAll, ok := inspect.Config.Labels[autoPublishAllLabel]; ok {
		if autoPA, _ := strconv.ParseBool(publishAll); autoPA {
			var pubcmd []string
			for port, _ := range inspect.Config.ExposedPorts {
				pubcmd = append(pubcmd, port.Port() + ":" + port.Port() + "/" + port.Proto())
			}
			if len(pubcmd) != 0 {
				pub := strings.Join(pubcmd, " ")
				cmd = append(cmd, pub)
				_, _ = fmt.Fprintf(stderr, "- [%s] Publish a container's port(s) to the host\n", pub)
			}
		}
	}

	cmd = append(cmd, copts.Image)

	if autoCmd, ok := inspect.Config.Labels[autoCmdLabel]; ok {
		if cmdArgs := strings.TrimSpace(autoCmd); cmdArgs != "" {
			args, err := parseCommandLine(cmdArgs)
			if err != nil {
				return err
			}
			copts.Args = args
			cmd = append(cmd, cmdArgs)
			_, _ = fmt.Fprintf(stderr, "- [%s] Command arguments to the container\n", cmdArgs)
		}
	}

	dockerCmd := strings.Join(cmd, " ")

	if !options.y {
		_, _ = fmt.Fprintf(stderr, `
the following command will be executed:
    %s

are you OK to proceed? ([y]/n) `, dockerCmd)
		var response string

		_, err = fmt.Scanln(&response)
		if err != nil && err.Error() != "expected newline" {
			return err
		}

		switch strings.ToLower(strings.TrimSpace(response)) {
		case "", "y", "yes":
			_, _ = fmt.Fprintln(stderr)
		default:
			return errors.New("canceled")
		}
	} else {
		_, _ = fmt.Fprintf(stderr, `
running:
    %s

`, dockerCmd)
	}

	containerConfig, err := parse(flags, copts, dockerCli.ServerInfo().OSType)
	if err != nil {
		reportError(dockerCli.Err(), "auto", err.Error(), true)
		return cli.StatusError{StatusCode: 125}
	}
	if err = validateAPIVersion(containerConfig, dockerCli.Client().ClientVersion()); err != nil {
		reportError(dockerCli.Err(), "auto", err.Error(), true)
		return cli.StatusError{StatusCode: 125}
	}

	runOpts := &runOptions{
		createOptions: createOptions{
			platform:  options.platform,
			untrusted: options.untrusted,
			pull:      PullImageNever, // already done by default or when inspecting the container
		},
		sigProxy: true,
	}

	return runContainer(dockerCli, runOpts, copts, containerConfig)
}
func parseCommandLine(command string) ([]string, error) {
	var args []string
	state := "start"
	current := ""
	quote := "\""
	escapeNext := true
	for i := 0; i < len(command); i++ {
		c := command[i]

		if state == "quotes" {
			if string(c) != quote {
				current += string(c)
			} else {
				args = append(args, current)
				current = ""
				state = "start"
			}
			continue
		}

		if escapeNext {
			current += string(c)
			escapeNext = false
			continue
		}

		if c == '\\' {
			escapeNext = true
			continue
		}

		if c == '"' || c == '\'' {
			state = "quotes"
			quote = string(c)
			continue
		}

		if state == "arg" {
			if c == ' ' || c == '\t' {
				args = append(args, current)
				current = ""
				state = "start"
			} else {
				current += string(c)
			}
			continue
		}

		if c != ' ' && c != '\t' {
			state = "arg"
			current += string(c)
		}
	}

	if state == "quotes" {
		return []string{}, errors.New(fmt.Sprintf("Unclosed quote in command line: %s", command))
	}

	if current != "" {
		args = append(args, current)
	}

	return args, nil
}
