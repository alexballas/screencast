#ifndef CAPTURE_WINDOWS_H
#define CAPTURE_WINDOWS_H

#include <stdint.h>
#include <stdbool.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef void (*WinVideoFrameCallback)(int id, void *data, uint32_t size, uint32_t width, uint32_t height);
typedef void (*WinAudioFrameCallback)(int id, void *data, uint32_t size);

void* InitWinCapture(int id, int streamIndex, bool includeAudio, WinVideoFrameCallback vcb, WinAudioFrameCallback acb);
void StartWinCapture(void* ctx);
void StopWinCapture(void* ctx);
void FreeWinCapture(void* ctx);

#ifdef __cplusplus
}
#endif

#endif
