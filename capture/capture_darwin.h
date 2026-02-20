#ifndef CAPTURE_DARWIN_H
#define CAPTURE_DARWIN_H

#include <stdint.h>
#include <stdbool.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef void (*VideoFrameCallback)(int id, void *data, uint32_t size, uint32_t width, uint32_t height);
typedef void (*AudioFrameCallback)(int id, void *data, uint32_t size);

void* InitMacCapture(int id, int streamIndex, bool includeAudio, VideoFrameCallback vcb, AudioFrameCallback acb);
void StartMacCapture(void* ctx);
void StopMacCapture(void* ctx);
void FreeMacCapture(void* ctx);

#ifdef __cplusplus
}
#endif

#endif
