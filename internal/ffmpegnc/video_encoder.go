// Package ffmpegnc selects the best available H.264 video encoder for a
// screencast pipeline, preferring hardware encoders and falling back to
// libx264. Shared by the hls and dlna packages so both streams always use the
// same codec decision.
package ffmpegnc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"go2tv.app/screencast/internal/debugutil"
	"go2tv.app/screencast/internal/processutil"
)

const probeTimeout = 5 * time.Second

// Plan is a concrete video encoding setup for ffmpeg.
type Plan struct {
	Label       string
	Codec       string
	Hardware    bool
	GlobalArgs  []string
	VideoFilter string
	CodecArgs   []string
}

// SelectVideoEncoder probes hardware candidates for the platform and returns
// the first that works, or the libx264 software plan. keyFrameIntervalSeconds
// drives the forced-keyframe cadence so both live streams and HLS segments open
// on an IDR frame.
func SelectVideoEncoder(ffmpegPath, baseFilter, gopArg string, keyFrameIntervalSeconds int, logOutput io.Writer, debug bool) Plan {
	software := softwarePlan(baseFilter, gopArg, keyFrameIntervalSeconds)

	candidates := hardwareCandidates(baseFilter, gopArg, keyFrameIntervalSeconds)
	if len(candidates) == 0 {
		reportSelection(logOutput, debug, software, "no_hardware_candidates")
		return software
	}

	if _, err := exec.LookPath(ffmpegPath); err != nil {
		if debug {
			debugutil.Printf("screencast/ffmpegnc encoder_probe ffmpeg_lookup_failed path=%q err=%v", ffmpegPath, err)
		}
		reportSelection(logOutput, debug, software, "ffmpeg_not_found")
		return software
	}

	available, encErr := ffmpegEncoderSet(ffmpegPath)
	if encErr != nil && debug {
		debugutil.Printf("screencast/ffmpegnc encoder_probe ffmpeg_encoders_failed err=%v", encErr)
	}

	for _, candidate := range candidates {
		if len(available) > 0 {
			if _, ok := available[candidate.Codec]; !ok {
				if debug {
					debugutil.Printf("screencast/ffmpegnc encoder_probe skip encoder=%q reason=not_in_ffmpeg_encoder_list", candidate.Label)
				}
				continue
			}
		}
		if err := probeEncoder(ffmpegPath, candidate); err == nil {
			reportSelection(logOutput, debug, candidate, "")
			return candidate
		} else if debug {
			debugutil.Printf("screencast/ffmpegnc encoder_probe failed encoder=%q err=%v", candidate.Label, err)
		}
	}

	reportSelection(logOutput, debug, software, "all_hardware_probes_failed")
	return software
}

func ffmpegEncoderSet(ffmpegPath string) (map[string]struct{}, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", "-encoders")
	processutil.HideConsoleWindow(cmd)
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("ffmpeg -encoders timeout after %s", probeTimeout)
	}
	if err != nil {
		return nil, fmt.Errorf("ffmpeg -encoders failed: %w", err)
	}

	encoders := make(map[string]struct{})
	lines := strings.SplitSeq(string(out), "\n")
	for line := range lines {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		// format is usually: " V..... h264_nvenc ...", where fields[0] is flags and fields[1] is encoder name.
		if strings.Contains(fields[0], "V") {
			encoders[fields[1]] = struct{}{}
		}
	}
	return encoders, nil
}

func reportSelection(logOutput io.Writer, debug bool, plan Plan, reason string) {
	mode := "software"
	if plan.Hardware {
		mode = "hardware"
	}

	msg := fmt.Sprintf("screencast video encoder: %s (%s)", plan.Label, mode)
	if logOutput != nil {
		_, _ = fmt.Fprintln(logOutput, msg)
	}
	if debug {
		if reason == "" {
			debugutil.Printf("screencast/ffmpegnc encoder selected=%q mode=%s", plan.Label, mode)
		} else {
			debugutil.Printf("screencast/ffmpegnc encoder selected=%q mode=%s reason=%s", plan.Label, mode, reason)
		}
	}
}

func probeEncoder(ffmpegPath string, plan Plan) error {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	args := []string{
		"-v", "error",
		"-nostdin",
	}
	args = append(args, plan.GlobalArgs...)
	args = append(args,
		"-f", "lavfi",
		"-i", "color=c=black:s=1280x720:r=30:d=0.5",
		"-an",
		"-frames:v", "8",
		"-r", "30",
	)
	if strings.TrimSpace(plan.VideoFilter) != "" {
		args = append(args, "-vf", plan.VideoFilter)
	}
	args = append(args, plan.CodecArgs...)
	args = append(args, "-f", "null", "-")

	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	processutil.HideConsoleWindow(cmd)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr

	err := cmd.Run()
	if ctx.Err() != nil {
		return fmt.Errorf("probe timeout after %s", probeTimeout)
	}
	if err != nil {
		return fmt.Errorf("probe failed: %w: %s", err, tailString(strings.TrimSpace(stderr.String()), 240))
	}
	return nil
}

func hardwareCandidates(baseFilter, gopArg string, keyFrameIntervalSeconds int) []Plan {
	switch runtime.GOOS {
	case "darwin":
		return []Plan{
			hardwarePlan("h264_videotoolbox", "h264_videotoolbox", nil, baseFilter+",format=yuv420p", gopArg, keyFrameIntervalSeconds),
		}
	case "windows":
		return []Plan{
			hardwarePlan("h264_nvenc", "h264_nvenc", nil, baseFilter+",format=yuv420p", gopArg, keyFrameIntervalSeconds),
			hardwarePlan("h264_amf", "h264_amf", nil, baseFilter+",format=yuv420p", gopArg, keyFrameIntervalSeconds),
			hardwarePlan("h264_qsv", "h264_qsv", nil, baseFilter+",format=nv12", gopArg, keyFrameIntervalSeconds),
		}
	default:
		candidates := []Plan{
			hardwarePlan("h264_nvenc", "h264_nvenc", nil, baseFilter+",format=yuv420p", gopArg, keyFrameIntervalSeconds),
		}

		devices, err := filepath.Glob("/dev/dri/renderD*")
		if err == nil {
			for _, dev := range devices {
				label := fmt.Sprintf("h264_vaapi (%s)", dev)
				candidates = append(candidates, hardwarePlan("h264_vaapi", label, []string{"-vaapi_device", dev}, baseFilter+",format=nv12,hwupload", gopArg, keyFrameIntervalSeconds))
			}
		}

		candidates = append(candidates, hardwarePlan("h264_qsv", "h264_qsv", nil, baseFilter+",format=nv12", gopArg, keyFrameIntervalSeconds))
		return candidates
	}
}

func hardwarePlan(codec, label string, globalArgs []string, filter, gopArg string, keyFrameIntervalSeconds int) Plan {
	return Plan{
		Label:       label,
		Codec:       codec,
		Hardware:    true,
		GlobalArgs:  append([]string(nil), globalArgs...),
		VideoFilter: filter,
		CodecArgs: []string{
			"-c:v", codec,
			"-b:v", "4000k",
			"-maxrate", "5000k",
			"-bufsize", "10000k",
			"-g", gopArg,
			"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", keyFrameIntervalSeconds),
		},
	}
}

func softwarePlan(baseFilter, gopArg string, keyFrameIntervalSeconds int) Plan {
	return Plan{
		Label:       "libx264",
		Codec:       "libx264",
		Hardware:    false,
		VideoFilter: baseFilter,
		CodecArgs: []string{
			"-c:v", "libx264",
			"-preset", "ultrafast",
			"-tune", "zerolatency",
			"-b:v", "4000k",
			"-maxrate", "5000k",
			"-bufsize", "10000k",
			"-pix_fmt", "yuv420p",
			"-g", gopArg,
			"-keyint_min", gopArg,
			"-sc_threshold", "0",
			"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", keyFrameIntervalSeconds),
		},
	}
}

func tailString(input string, max int) string {
	if input == "" {
		return "no ffmpeg stderr output"
	}
	if max <= 0 || len(input) <= max {
		return input
	}
	return input[len(input)-max:]
}
