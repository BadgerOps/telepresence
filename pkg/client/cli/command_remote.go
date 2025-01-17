package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	empty "google.golang.org/protobuf/types/known/emptypb"

	"github.com/datawire/dlib/dlog"
	"github.com/telepresenceio/telepresence/rpc/v2/connector"
	"github.com/telepresenceio/telepresence/v2/pkg/client/cli/ann"
	"github.com/telepresenceio/telepresence/v2/pkg/client/cli/cliutil"
	"github.com/telepresenceio/telepresence/v2/pkg/client/cli/output"
	"github.com/telepresenceio/telepresence/v2/pkg/client/errcat"
	"github.com/telepresenceio/telepresence/v2/pkg/client/userd/commands"
	"github.com/telepresenceio/telepresence/v2/pkg/proc"
)

func getRemoteCommands(cmd *cobra.Command, forceStart bool) (groups cliutil.CommandGroups, err error) {
	groups = make(cliutil.CommandGroups)
	av := ann.Optional
	if forceStart {
		av = ann.Required
	}
	cmd.Annotations = map[string]string{ann.UserDaemon: av}
	if err := cliutil.InitCommand(cmd); err != nil {
		return nil, err
	}
	ctx := cmd.Context()
	if userD := cliutil.GetUserDaemon(ctx); userD != nil {
		remote, err := userD.ListCommands(ctx, &empty.Empty{})
		if err != nil {
			return nil, fmt.Errorf("unable to call ListCommands: %w", err)
		}
		funcBundle := cliutil.CommandFuncBundle{
			RunE:              runRemote,
			ValidArgsFunction: validArgsFuncRemote,
		}
		if groups, err = cliutil.RPCToCommands(remote, funcBundle); err != nil {
			groups = commands.GetCommandsForLocal(ctx, err)
		}
		userDaemonRunning = true
	}
	return groups, err
}

func initRemoteCommand(cmd *cobra.Command) error {
	ca := cmd.Annotations
	if ca == nil {
		ca = make(map[string]string)
		cmd.Annotations = ca
	}
	ca[ann.Session] = ann.Required
	ca[ann.RootDaemon] = ann.Required
	return cliutil.InitCommand(cmd)
}

func validArgsFuncRemote(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if err := initRemoteCommand(cmd); err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	ctx := cmd.Context()
	resp, err := cliutil.GetUserDaemon(ctx).ValidArgsForCommand(ctx, &connector.ValidArgsForCommandRequest{
		CmdName:    cmd.Name(),
		OsArgs:     args,
		ToComplete: toComplete,
	})
	if err != nil {
		return []string{}, 0
	}
	return resp.Completions, cobra.ShellCompDirective(resp.ShellCompDirective)
}

func stdinPump(ctx context.Context, cmdStream connector.Connector_RunCommandClient, cmd *cobra.Command) {
	buf := make([]byte, 1024)
	stdin := cmd.InOrStdin()
	for ctx.Err() == nil {
		n, err := stdin.Read(buf)
		if n > 0 {
			if err = cmdStream.Send(&connector.RunCommandRequest{COrD: &connector.RunCommandRequest_Data{Data: buf[:n]}}); err != nil {
				if ctx.Err() == nil {
					dlog.Errorf(ctx, "failed to forward to stdin: %v\n", err)
				}
				return
			}
		}
		if err != nil {
			if !(errors.Is(err, io.EOF) || ctx.Err() != nil) {
				dlog.Errorf(ctx, "failed to read from stdin: %v\n", err)
			}
			return
		}
	}
}

func interruptPump(ctx context.Context, cmdStream connector.Connector_RunCommandClient, cancel context.CancelFunc) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, proc.SignalsToForward...)
	defer func() {
		signal.Stop(sigCh)
		close(sigCh)
	}()

	select {
	case <-ctx.Done():
	case sig := <-sigCh:
		if sig == nil {
			return
		}
		err := cmdStream.Send(&connector.RunCommandRequest{COrD: &connector.RunCommandRequest_SoftCancel{SoftCancel: true}})
		if err != nil {
			if ctx.Err() != nil {
				dlog.Errorf(ctx, "failed to send soft cancel: %v\n", err)
			}
			return
		}
		// Trigger "hard" cancel if needed.
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			cancel()
		}
	}
}

func stdoutAndStderrPump(ctx context.Context, cmdStream connector.Connector_RunCommandClient, cmd *cobra.Command) error {
	// We don't use structured output here because that's being taking care of remotely.
	stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
	defer cmdStream.CloseSend()
	for ctx.Err() == nil {
		sr, err := cmdStream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				// Normal command termination
				return nil
			}
			return fmt.Errorf("failed to read stdout/stderr stream: %w\n", err)
		}
		r := sr.Data
		if sr.Final {
			// Command execution ended with an error
			if r != nil {
				err = errcat.FromResult(r)
			}
			return err
		}

		// Normal output from the command
		var w io.Writer
		if r.ErrorCategory == 0 {
			w = stdout
		} else {
			w = stderr
		}
		if _, err = w.Write(r.Data); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("failed to write stdout/stderr: %w\n", err)
		}
	}
	return nil
}

func runRemote(cmd *cobra.Command, args []string) error {
	if err := initRemoteCommand(cmd); err != nil {
		return err
	}
	ctx := cmd.Context()
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	userD := cliutil.GetUserDaemon(ctx)
	// Use a graceful termination period
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	_, stderr := output.Structured(ctx)

	cmdStream, err := userD.RunCommand(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "failed start command: %v\n", err)
		return err
	}

	// FlagParsing is disabled on the local-side cmd so args is actually going to hold flags and args both
	// Thus command_name + args is the entire command line (except for the "telepresence" string in os.Args[0])
	err = cmdStream.Send(&connector.RunCommandRequest{
		COrD: &connector.RunCommandRequest_Command_{Command: &connector.RunCommandRequest_Command{
			OsArgs: append([]string{cmd.CalledAs()}, args...),
			Cwd:    cwd,
		}},
	})
	if err != nil {
		fmt.Fprintf(stderr, "failed to send: %v\n", err)
		return err
	}

	// Start all pumps, wait for the stdout/stderr pump to finish
	go stdinPump(ctx, cmdStream, cmd)
	go interruptPump(ctx, cmdStream, cancel)
	return stdoutAndStderrPump(ctx, cmdStream, cmd)
}
