#import <Foundation/Foundation.h>
#import <ScreenCaptureKit/ScreenCaptureKit.h>
#import <CoreMedia/CoreMedia.h>
#import <CoreVideo/CoreVideo.h>
#import <CoreGraphics/CoreGraphics.h>
#import "capture_darwin.h"

static SCShareableContent *GetShareableContentSync(void) {
    dispatch_semaphore_t sem = dispatch_semaphore_create(0);
    __block SCShareableContent *shareableContent = nil;
    [SCShareableContent getShareableContentWithCompletionHandler:^(SCShareableContent *content, NSError *error) {
        if (error == nil) {
            shareableContent = content;
        }
        dispatch_semaphore_signal(sem);
    }];
    dispatch_semaphore_wait(sem, DISPATCH_TIME_FOREVER);
    return shareableContent;
}

int GetMacDisplayCount(void) {
    SCShareableContent *shareableContent = GetShareableContentSync();
    if (!shareableContent) {
        return 0;
    }
    return (int)shareableContent.displays.count;
}

bool GetMacDisplayInfo(int index, uint32_t *displayID, uint32_t *width, uint32_t *height, bool *isPrimary) {
    SCShareableContent *shareableContent = GetShareableContentSync();
    if (!shareableContent) {
        return false;
    }
    if (index < 0 || index >= (int)shareableContent.displays.count) {
        return false;
    }

    SCDisplay *display = shareableContent.displays[index];
    if (displayID) {
        *displayID = (uint32_t)display.displayID;
    }
    if (width) {
        *width = (uint32_t)display.width;
    }
    if (height) {
        *height = (uint32_t)display.height;
    }
    if (isPrimary) {
        *isPrimary = ((uint32_t)CGMainDisplayID() == (uint32_t)display.displayID);
    }
    return true;
}

@interface MacCaptureSession : NSObject <SCStreamDelegate, SCStreamOutput>
@property (nonatomic, assign) int goID;
@property (nonatomic, assign) VideoFrameCallback videoCallback;
@property (nonatomic, assign) AudioFrameCallback audioCallback;
@property (nonatomic, strong) SCStream *stream;
@property (nonatomic, strong) dispatch_queue_t queue;
@property (nonatomic, assign) BOOL includeAudio;
@property (nonatomic, assign) uint8_t *packedBuf;
@property (nonatomic, assign) size_t packedBufSize;
@end

@implementation MacCaptureSession

- (void)dealloc {
    if (_packedBuf) {
        free(_packedBuf);
        _packedBuf = NULL;
    }
}

- (instancetype)initWithID:(int)goID streamIndex:(int)streamIndex includeAudio:(BOOL)includeAudio vcb:(VideoFrameCallback)vcb acb:(AudioFrameCallback)acb {
    self = [super init];
    if (self) {
        self.goID = goID;
        self.videoCallback = vcb;
        self.audioCallback = acb;
        self.includeAudio = includeAudio;
        self.queue = dispatch_queue_create("screencast_mac", DISPATCH_QUEUE_SERIAL);
        
        SCShareableContent *shareableContent = GetShareableContentSync();
        
        if (!shareableContent || shareableContent.displays.count == 0) {
            return nil;
        }
        
        SCDisplay *targetDisplay = shareableContent.displays.firstObject;
        if (streamIndex >= 0 && streamIndex < shareableContent.displays.count) {
            targetDisplay = shareableContent.displays[streamIndex];
        }
        
        SCStreamConfiguration *config = [[SCStreamConfiguration alloc] init];
        config.width = targetDisplay.width;
        config.height = targetDisplay.height;
        config.showsCursor = YES;
        config.pixelFormat = kCVPixelFormatType_32BGRA;
        config.queueDepth = 8;
        
        if (@available(macOS 13.0, *)) {
            config.capturesAudio = includeAudio;
            if (includeAudio) {
                config.excludesCurrentProcessAudio = YES;
                config.sampleRate = 48000;
                config.channelCount = 2;
            }
        }
        
        SCContentFilter *filter = [[SCContentFilter alloc] initWithDisplay:targetDisplay excludingWindows:@[]];
        
        self.stream = [[SCStream alloc] initWithFilter:filter configuration:config delegate:self];
        NSError *err = nil;
        [self.stream addStreamOutput:self type:SCStreamOutputTypeScreen sampleHandlerQueue:self.queue error:&err];
        
        if (includeAudio) {
            if (@available(macOS 13.0, *)) {
                [self.stream addStreamOutput:self type:SCStreamOutputTypeAudio sampleHandlerQueue:self.queue error:&err];
            }
        }
    }
    return self;
}

- (void)start {
    [self.stream startCaptureWithCompletionHandler:^(NSError *error) {
        (void)error;
    }];
}

- (void)stop {
    [self.stream stopCaptureWithCompletionHandler:^(NSError *error) {
        (void)error;
    }];
}

- (void)stream:(SCStream *)stream didOutputSampleBuffer:(CMSampleBufferRef)sampleBuffer ofType:(SCStreamOutputType)type {
    if (type == SCStreamOutputTypeScreen) {
        CVImageBufferRef imageBuffer = CMSampleBufferGetImageBuffer(sampleBuffer);
        if (!imageBuffer) return;

        CVPixelBufferLockBaseAddress(imageBuffer, kCVPixelBufferLock_ReadOnly);
        void *baseAddress = CVPixelBufferGetBaseAddress(imageBuffer);
        size_t bytesPerRow = CVPixelBufferGetBytesPerRow(imageBuffer);
        size_t width = CVPixelBufferGetWidth(imageBuffer);
        size_t height = CVPixelBufferGetHeight(imageBuffer);

        if (baseAddress && self.videoCallback) {
            size_t rowBytes = width * 4;
            size_t packedSize = rowBytes * height;

            if (bytesPerRow == rowBytes) {
                // No row padding — pass buffer directly.
                self.videoCallback(self.goID, baseAddress, (uint32_t)packedSize, (uint32_t)width, (uint32_t)height);
            } else {
                // Row padding present — strip it so FFmpeg sees tightly-packed rows.
                if (self.packedBufSize < packedSize) {
                    free(self.packedBuf);
                    self.packedBuf = (uint8_t *)malloc(packedSize);
                    self.packedBufSize = self.packedBuf ? packedSize : 0;
                }
                if (self.packedBuf) {
                    const uint8_t *src = (const uint8_t *)baseAddress;
                    for (size_t y = 0; y < height; y++) {
                        memcpy(self.packedBuf + y * rowBytes, src + y * bytesPerRow, rowBytes);
                    }
                    self.videoCallback(self.goID, self.packedBuf, (uint32_t)packedSize, (uint32_t)width, (uint32_t)height);
                }
            }
        }
        CVPixelBufferUnlockBaseAddress(imageBuffer, kCVPixelBufferLock_ReadOnly);
    } else {
        if (@available(macOS 13.0, *)) {
            if (type == SCStreamOutputTypeAudio) {
                CMBlockBufferRef blockBuffer = CMSampleBufferGetDataBuffer(sampleBuffer);
                if (!blockBuffer) return;
                
                size_t lengthAtOffset, totalLength;
                char *dataPointer;
                OSStatus status = CMBlockBufferGetDataPointer(blockBuffer, 0, &lengthAtOffset, &totalLength, &dataPointer);
                
                if (status == kCMBlockBufferNoErr && dataPointer && self.audioCallback) {
                    self.audioCallback(self.goID, dataPointer, (uint32_t)totalLength);
                }
            }
        }
    }
}

@end

void* InitMacCapture(int id, int streamIndex, bool includeAudio, VideoFrameCallback vcb, AudioFrameCallback acb) {
    MacCaptureSession *session = [[MacCaptureSession alloc] initWithID:id streamIndex:streamIndex includeAudio:includeAudio vcb:vcb acb:acb];
    if (session) {
        return (__bridge_retained void*)session;
    }
    return NULL;
}

void StartMacCapture(void* ctx) {
    if (!ctx) return;
    MacCaptureSession *session = (__bridge MacCaptureSession*)ctx;
    [session start];
}

void StopMacCapture(void* ctx) {
    if (!ctx) return;
    MacCaptureSession *session = (__bridge MacCaptureSession*)ctx;
    [session stop];
}

void FreeMacCapture(void* ctx) {
    if (!ctx) return;
    MacCaptureSession *session = (__bridge_transfer MacCaptureSession*)ctx;
    session = nil;
}
