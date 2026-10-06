// Package ffmpeg starts ffmpeg processes for transcoding, either into a pipe
// (live streaming) or a file.
package ffmpeg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

// Pipe is the Spec.Output value that sends ffmpeg's output to Proc.Stdout.
const Pipe = "pipe:1"

type Spec struct {
	// Inputs are local paths or URLs, passed as successive -i arguments
	// (e.g. separate video and audio streams to be muxed together).
	Inputs []string
	// Offset seeks every input before decoding starts.
	Offset time.Duration
	// OutArgs go between the inputs and Output: maps, codecs, container.
	OutArgs []string
	// Output is a file path or Pipe.
	Output string
}

// Proc is a running ffmpeg. Read Stdout to EOF (Pipe output only), then Wait.
type Proc struct {
	Stdout io.ReadCloser // nil unless Output == Pipe

	cmd    *exec.Cmd
	stderr *bytes.Buffer
	log    *slog.Logger
	ctx    context.Context
}

func isURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// Args builds the full ffmpeg argv for spec.
func Args(spec Spec) []string {
	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-nostdin",
		"-y",
	}
	for _, in := range spec.Inputs {
		// Reconnect flags are useful mainly for URL sources.
		if isURL(in) {
			args = append(args,
				"-reconnect", "1",
				"-reconnect_streamed", "1",
				"-reconnect_on_network_error", "1",
				"-reconnect_on_http_error", "4xx,5xx",
				"-reconnect_delay_max", "5",
				// Error out instead of hanging on a dead connection so the
				// caller's retry logic can take over.
				"-rw_timeout", "15000000", // 15 s, in µs
			)
		}
		if spec.Offset > 0 {
			args = append(args, "-ss", fmt.Sprintf("%.2f", spec.Offset.Seconds()))
		}
		args = append(args, "-i", in)
	}
	args = append(args, spec.OutArgs...)
	return append(args, spec.Output)
}

// Start launches ffmpeg. The process is killed when ctx is cancelled.
func Start(ctx context.Context, l *slog.Logger, spec Spec) (*Proc, error) {
	if len(spec.Inputs) == 0 {
		return nil, errors.New("ffmpeg: no inputs")
	}
	if spec.Output == "" {
		return nil, errors.New("ffmpeg: no output")
	}
	args := Args(spec)

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	p := &Proc{cmd: cmd, stderr: stderr, log: l, ctx: ctx}

	if spec.Output == Pipe {
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			l.Error("ffmpeg stdout pipe failed", slog.Any("err", err))
			return nil, fmt.Errorf("stdout pipe: %w", err)
		}
		p.Stdout = stdout
	}
	if err := cmd.Start(); err != nil {
		l.Error("ffmpeg start failed",
			slog.Any("err", err), slog.String("stderr", stderr.String()))
		return nil, fmt.Errorf("ffmpeg start: %w | %s", err, stderr.String())
	}

	// The exact argv is what lets a failure be reproduced by hand. Googlevideo
	// URLs carry signed tokens and run to thousands of characters, so inputs
	// are reduced to their host.
	labels := make([]string, len(spec.Inputs))
	for i, in := range spec.Inputs {
		labels[i] = sourceLabel(in)
	}
	l.Debug("ffmpeg started",
		slog.Int("pid", cmd.Process.Pid),
		slog.Any("inputs", labels),
		slog.Duration("seek", spec.Offset),
		slog.Any("args", redactedArgs(args, spec.Inputs)))
	return p, nil
}

// Kill stops ffmpeg early; Wait must still be called to reap it.
func (p *Proc) Kill() {
	if err := p.cmd.Process.Kill(); err != nil {
		p.log.Debug("killing ffmpeg failed", slog.Any("err", err))
	}
}

// Wait reaps ffmpeg and logs its exit status and diagnostics. A non-zero
// exit is returned with ffmpeg's stderr attached; if ctx was cancelled, the
// context error is returned instead of the resulting "signal: killed".
func (p *Proc) Wait() error {
	waitErr := p.cmd.Wait()

	exitCode := -1
	if p.cmd.ProcessState != nil {
		exitCode = p.cmd.ProcessState.ExitCode()
	}
	msg := strings.TrimSpace(p.stderr.String())
	// Always surface ffmpeg's own diagnostics: a clean-looking exit that
	// logged errors should still leave a trace.
	if msg != "" {
		p.log.Warn("ffmpeg stderr", slog.Int("exit_code", exitCode), slog.String("output", msg))
	}
	p.log.Debug("ffmpeg exited", slog.Int("exit_code", exitCode), slog.Any("wait_err", waitErr))

	if err := p.ctx.Err(); err != nil {
		return err
	}
	if waitErr != nil {
		if msg != "" {
			return fmt.Errorf("ffmpeg: %w | %s", waitErr, msg)
		}
		return fmt.Errorf("ffmpeg: %w", waitErr)
	}
	return nil
}

// Run executes spec (whose Output must be a file) to completion.
func Run(ctx context.Context, l *slog.Logger, spec Spec) error {
	if spec.Output == Pipe {
		return errors.New("ffmpeg.Run: use Start for pipe output")
	}
	p, err := Start(ctx, l, spec)
	if err != nil {
		return err
	}
	return p.Wait()
}

// redactedArgs returns the argv with each input replaced by its host, so the
// command can be read (and reconstructed) without printing a signed URL.
func redactedArgs(args, inputs []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i, a := range out {
		for _, in := range inputs {
			if a == in {
				out[i] = sourceLabel(in)
			}
		}
	}
	return out
}

// sourceLabel reduces a URL to its host for logs; non-URLs pass through.
func sourceLabel(raw string) string {
	if !isURL(raw) {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "?"
	}
	return u.Host
}
