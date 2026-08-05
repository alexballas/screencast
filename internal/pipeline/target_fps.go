package pipeline

import "go2tv.app/screencast/capture"

const (
	defaultMaxFrameRate  = 60
	defaultHighResCapFPS = 30
)

// TargetFPS is the rate the pacer holds the video timeline to: whatever the
// source reports, capped at defaultMaxFrameRate and lowered again above 1080p.
func TargetFPS(stream *capture.Stream) uint32 {
	frameRate := stream.FrameRate
	if frameRate == 0 {
		frameRate = defaultMaxFrameRate
	}

	target := frameRate
	if target > defaultMaxFrameRate {
		target = defaultMaxFrameRate
	}
	if stream.Width*stream.Height > 1920*1080 && target > defaultHighResCapFPS {
		target = defaultHighResCapFPS
	}

	return target
}
