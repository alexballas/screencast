package pipeline

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

	"go2tv.app/screencast/internal/processutil"
)

const encoderProbeTimeout = 5 * time.Second

type VideoEncoderPlan struct {
	Label       string
	Codec       string
	Hardware    bool
	GlobalArgs  []string
	VideoFilter string
	CodecArgs   []string
}

// SelectVideoEncoder returns the best encoder plan this machine can actually
// run: the first hardware candidate that probes clean, or libx264.
//
// gopArg and keyframeSeconds are the same keyframe interval expressed twice,
// and both are the caller's to decide. gopArg is the GOP length in frames,
// which -g and -keyint_min ask the encoder for; keyframeSeconds pins the
// keyframes to the timeline instead, via -force_key_frames, so that whatever
// consumes the output can cut at that interval regardless of what the encoder
// chose to do about scene changes. What a sensible interval is depends on the
// output format - a segment length for HLS, something else for a continuous
// stream - so nothing here assumes one. Seconds below 1 make the expression
// force a keyframe on every frame; callers normalise their own options.
func SelectVideoEncoder(ffmpegPath, baseFilter, gopArg string, keyframeSeconds int, logOutput io.Writer, debug bool) VideoEncoderPlan {
	software := softwareEncoderPlan(baseFilter, gopArg, keyframeSeconds)

	candidates := hardwareEncoderCandidates(baseFilter, gopArg, keyframeSeconds)
	if len(candidates) == 0 {
		reportEncoderSelection(logOutput, debug, software, "no_hardware_candidates")
		return software
	}

	if _, err := exec.LookPath(ffmpegPath); err != nil {
		if debug {
			DebugPrintf("screencast/pipeline encoder_probe ffmpeg_lookup_failed path=%q err=%v", ffmpegPath, err)
		}
		reportEncoderSelection(logOutput, debug, software, "ffmpeg_not_found")
		return software
	}

	available, encErr := ffmpegEncoderSet(ffmpegPath)
	if encErr != nil && debug {
		DebugPrintf("screencast/pipeline encoder_probe ffmpeg_encoders_failed err=%v", encErr)
	}

	for _, candidate := range candidates {
		if len(available) > 0 {
			if _, ok := available[candidate.Codec]; !ok {
				if debug {
					DebugPrintf("screencast/pipeline encoder_probe skip encoder=%q reason=not_in_ffmpeg_encoder_list", candidate.Label)
				}
				continue
			}
		}
		if err := probeVideoEncoder(ffmpegPath, candidate); err == nil {
			reportEncoderSelection(logOutput, debug, candidate, "")
			return candidate
		} else if debug {
			DebugPrintf("screencast/pipeline encoder_probe failed encoder=%q err=%v", candidate.Label, err)
		}
	}

	reportEncoderSelection(logOutput, debug, software, "all_hardware_probes_failed")
	return software
}

func ffmpegEncoderSet(ffmpegPath string) (map[string]struct{}, error) {
	ctx, cancel := context.WithTimeout(context.Background(), encoderProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", "-encoders")
	processutil.HideConsoleWindow(cmd)
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("ffmpeg -encoders timeout after %s", encoderProbeTimeout)
	}
	if err != nil {
		return nil, fmt.Errorf("ffmpeg -encoders failed: %w", err)
	}

	encoders := make(map[string]struct{})
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
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

func reportEncoderSelection(logOutput io.Writer, debug bool, plan VideoEncoderPlan, reason string) {
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
			DebugPrintf("screencast/pipeline encoder selected=%q mode=%s", plan.Label, mode)
		} else {
			DebugPrintf("screencast/pipeline encoder selected=%q mode=%s reason=%s", plan.Label, mode, reason)
		}
	}
}

func probeVideoEncoder(ffmpegPath string, plan VideoEncoderPlan) error {
	ctx, cancel := context.WithTimeout(context.Background(), encoderProbeTimeout)
	defer cancel()

	args := []string{
		"-v", "error",
		"-nostdin",
	}
	args = append(args, plan.GlobalArgs...)
	args = append(
		args,
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
		return fmt.Errorf("probe timeout after %s", encoderProbeTimeout)
	}
	if err != nil {
		return fmt.Errorf("probe failed: %w: %s", err, tailString(strings.TrimSpace(stderr.String()), 240))
	}
	return nil
}

func hardwareEncoderCandidates(baseFilter, gopArg string, keyframeSeconds int) []VideoEncoderPlan {
	switch runtime.GOOS {
	case "darwin":
		return []VideoEncoderPlan{
			hardwareEncoderPlan("h264_videotoolbox", "h264_videotoolbox", nil, baseFilter+",format=yuv420p", gopArg, keyframeSeconds),
		}
	case "windows":
		return []VideoEncoderPlan{
			hardwareEncoderPlan("h264_nvenc", "h264_nvenc", nil, baseFilter+",format=yuv420p", gopArg, keyframeSeconds),
			hardwareEncoderPlan("h264_amf", "h264_amf", nil, baseFilter+",format=yuv420p", gopArg, keyframeSeconds),
			hardwareEncoderPlan("h264_qsv", "h264_qsv", nil, baseFilter+",format=nv12", gopArg, keyframeSeconds),
		}
	default:
		candidates := []VideoEncoderPlan{
			hardwareEncoderPlan("h264_nvenc", "h264_nvenc", nil, baseFilter+",format=yuv420p", gopArg, keyframeSeconds),
		}

		devices, err := filepath.Glob("/dev/dri/renderD*")
		if err == nil {
			for _, dev := range devices {
				label := fmt.Sprintf("h264_vaapi (%s)", dev)
				candidates = append(candidates, hardwareEncoderPlan("h264_vaapi", label, []string{"-vaapi_device", dev}, baseFilter+",format=nv12,hwupload", gopArg, keyframeSeconds))
			}
		}

		candidates = append(candidates, hardwareEncoderPlan("h264_qsv", "h264_qsv", nil, baseFilter+",format=nv12", gopArg, keyframeSeconds))
		return candidates
	}
}

func hardwareEncoderPlan(codec, label string, globalArgs []string, filter, gopArg string, keyframeSeconds int) VideoEncoderPlan {
	return VideoEncoderPlan{
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
			"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", keyframeSeconds),
		},
	}
}

func softwareEncoderPlan(baseFilter, gopArg string, keyframeSeconds int) VideoEncoderPlan {
	return VideoEncoderPlan{
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
			"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", keyframeSeconds),
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
