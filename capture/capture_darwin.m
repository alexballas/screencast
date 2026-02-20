#import <Foundation/Foundation.h>
#import <ScreenCaptureKit/ScreenCaptureKit.h>
#import <CoreMedia/CoreMedia.h>
#import <CoreVideo/CoreVideo.h>
#import "capture_darwin.h"

@interface MacCaptureSession : NSObject <SCStreamDelegate, SCStreamOutput>
@property (nonatomic, assign) int goID;
@property (nonatomic, assign) VideoFrameCallback videoCallback;
@property (nonatomic, assign) AudioFrameCallback audioCallback;
@property (nonatomic, strong) SCStream *stream;
@property (nonatomic, strong) dispatch_queue_t queue;
@property (nonatomic, assign) BOOL isRunning;
@property (nonatomic, assign) BOOL includeAudio;
@end

@implementation MacCaptureSession

- (instancetype)initWithID:(int)goID streamIndex:(int)streamIndex includeAudio:(BOOL)includeAudio vcb:(VideoFrameCallback)vcb acb:(AudioFrameCallback)acb {
    self = [super init];
    if (self) {
        self.goID = goID;
        self.videoCallback = vcb;
        self.audioCallback = acb;
        self.includeAudio = includeAudio;
        self.queue = dispatch_queue_create("screencast_mac", DISPATCH_QUEUE_SERIAL);
        
        dispatch_semaphore_t sem = dispatch_semaphore_create(0);
        __block SCShareableContent *shareableContent = nil;
        
        [SCShareableContent getShareableContentWithCompletionHandler:^(SCShareableContent *content, NSError *error) {
            shareableContent = content;
            dispatch_semaphore_signal(sem);
        }];
        
        dispatch_semaphore_wait(sem, DISPATCH_TIME_FOREVER);
        
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
        
        if (@available(macOS 13.0, *)) {
            config.capturesAudio = includeAudio;
            if (includeAudio) {
                config.sampleRate = 48000;
                config.channelCount = 2;
            }
        }
        
        SCDisplayFilter *filter = [[SCDisplayFilter alloc] initWithDisplay:targetDisplay excludingWindows:@[]];
        
        self.stream = [[SCStream alloc] initWithFilter:filter configuration:config delegate:self];
        NSError *err = nil;
        [self.stream addStreamOutput:self type:SCStreamOutputTypeScreen sampleHandlerQueue:self.queue error:&err];
        
        if (includeAudio) {
            [self.stream addStreamOutput:self type:SCStreamOutputTypeAudio sampleHandlerQueue:self.queue error:&err];
        }
    }
    return self;
}

- (void)start {
    [self.stream startCaptureWithCompletionHandler:^(NSError *error) {
        if (!error) {
            self.isRunning = YES;
        }
    }];
}

- (void)stop {
    [self.stream stopCaptureWithCompletionHandler:^(NSError *error) {
        self.isRunning = NO;
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
            self.videoCallback(self.goID, baseAddress, (uint32_t)(bytesPerRow * height), (uint32_t)width, (uint32_t)height);
        }
        CVPixelBufferUnlockBaseAddress(imageBuffer, kCVPixelBufferLock_ReadOnly);
    } else if (type == SCStreamOutputTypeAudio) {
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
