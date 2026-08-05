// Package pipeline holds the capture-to-ffmpeg machinery that every output
// format shares: encoder selection, the frame pacer and audio relay that keep
// the two ffmpeg input timelines on one clock, and the argument vector up to
// but not including the muxer.
//
// Everything here is upstream of the muxer. What a caller adds is the muxer
// arguments and whatever it does with the output.
package pipeline
