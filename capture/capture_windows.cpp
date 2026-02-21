#include "capture_windows.h"
#include <windows.h>
#include <winrt/Windows.Foundation.h>
#include <winrt/Windows.System.h>
#include <winrt/Windows.Graphics.Capture.h>
#include <winrt/Windows.Graphics.DirectX.h>
#include <winrt/Windows.Graphics.DirectX.Direct3D11.h>
#include <windows.graphics.capture.interop.h>
#if __has_include(<windows.graphics.directx.direct3d11.interop.h>)
#include <windows.graphics.directx.direct3d11.interop.h>
#else
// MinGW/MSYS2 package lag: this header may be missing. Keep equivalent declarations local.
struct IDirect3DDxgiInterfaceAccess : IUnknown {
	virtual HRESULT STDMETHODCALLTYPE GetInterface(REFIID id, void **object) = 0;
};

extern "C" HRESULT __stdcall CreateDirect3D11DeviceFromDXGIDevice(IUnknown *dxgiDevice, IInspectable **graphicsDevice);
extern "C" HRESULT __stdcall CreateDirect3D11SurfaceFromDXGISurface(IUnknown *dxgiSurface, IInspectable **graphicsSurface);
#endif
#include <d3d11.h>
#include <dxgi1_2.h>
#include <mmdeviceapi.h>
#include <audioclient.h>
#include <thread>
#include <atomic>
#include <vector>

using namespace winrt;
using namespace Windows::Graphics::Capture;
using namespace Windows::Graphics::DirectX;
using namespace Windows::Graphics::DirectX::Direct3D11;

// Required for COM interfaces
#pragma comment(lib, "windowsapp")
#pragma comment(lib, "d3d11")

struct WinCaptureSession {
    int goID;
    WinVideoFrameCallback videoCallback;
    WinAudioFrameCallback audioCallback;
    bool includeAudio;
    
    std::atomic<bool> isRunning{false};
    std::thread audioThread;
    
    winrt::com_ptr<ID3D11Device> d3dDevice;
    winrt::com_ptr<ID3D11DeviceContext> d3dContext;
    IDirect3DDevice device{nullptr};
    GraphicsCaptureItem item{nullptr};
    Direct3D11CaptureFramePool framePool{nullptr};
    GraphicsCaptureSession session{nullptr};
    
    // Audio
    winrt::com_ptr<IAudioClient> audioClient;
    winrt::com_ptr<IAudioCaptureClient> captureClient;
    HANDLE audioEvent{nullptr};
};

static inline auto CreateD3DDevice() {
    winrt::com_ptr<ID3D11Device> d3dDevice;
    UINT flags = D3D11_CREATE_DEVICE_BGRA_SUPPORT;
    D3D_FEATURE_LEVEL featureLevels[] = { D3D_FEATURE_LEVEL_11_0 };
    
    D3D11CreateDevice(nullptr, D3D_DRIVER_TYPE_HARDWARE, nullptr, flags,
        featureLevels, 1, D3D11_SDK_VERSION, d3dDevice.put(), nullptr, nullptr);
    return d3dDevice;
}

static inline auto CreateDirect3DDevice(ID3D11Device* d3dDevice) {
    winrt::com_ptr<IDXGIDevice> dxgiDevice;
    d3dDevice->QueryInterface(__uuidof(IDXGIDevice), dxgiDevice.put_void());
    winrt::com_ptr<::IInspectable> device;
    CreateDirect3D11DeviceFromDXGIDevice(dxgiDevice.get(), device.put());
    return device.as<IDirect3DDevice>();
}

// Enumerate monitors helper
struct MonitorInfo {
    HMONITOR hMonitor;
    RECT rect;
};

static BOOL CALLBACK MonitorEnumProc(HMONITOR hMonitor, HDC, LPRECT lprcMonitor, LPARAM dwData) {
    auto monitors = reinterpret_cast<std::vector<MonitorInfo>*>(dwData);
    monitors->push_back({ hMonitor, *lprcMonitor });
    return TRUE;
}

void AudioCaptureLoop(WinCaptureSession* sess) {
    HRESULT hr = CoInitializeEx(nullptr, COINIT_MULTITHREADED);
    if (FAILED(hr)) return;

    UINT32 packetLength = 0;
    while (sess->isRunning) {
        DWORD waitResult = WaitForSingleObject(sess->audioEvent, 1000);
        if (waitResult != WAIT_OBJECT_0) continue;
        
        hr = sess->captureClient->GetNextPacketSize(&packetLength);
        if (FAILED(hr)) break;
        
        while (packetLength != 0) {
            BYTE* data;
            UINT32 numFramesAvailable;
            DWORD flags;
            
            hr = sess->captureClient->GetBuffer(&data, &numFramesAvailable, &flags, nullptr, nullptr);
            if (FAILED(hr)) break;
            
            if (sess->audioCallback && numFramesAvailable > 0) {
                // Assuming 48kHz, 16-bit, stereo as per unified requirement. 
                // Actual conversion might be needed if format differs, but we send raw for now.
                // 1 frame = 2 channels * 2 bytes = 4 bytes.
                sess->audioCallback(sess->goID, data, numFramesAvailable * 4);
            }
            
            hr = sess->captureClient->ReleaseBuffer(numFramesAvailable);
            if (FAILED(hr)) break;
            
            hr = sess->captureClient->GetNextPacketSize(&packetLength);
            if (FAILED(hr)) break;
        }
    }
    CoUninitialize();
}

void* InitWinCapture(int id, int streamIndex, bool includeAudio, WinVideoFrameCallback vcb, WinAudioFrameCallback acb) {
    winrt::init_apartment(winrt::apartment_type::multi_threaded);
    
    WinCaptureSession* sess = new WinCaptureSession();
    sess->goID = id;
    sess->videoCallback = vcb;
    sess->audioCallback = acb;
    sess->includeAudio = includeAudio;
    
    std::vector<MonitorInfo> monitors;
    EnumDisplayMonitors(nullptr, nullptr, MonitorEnumProc, reinterpret_cast<LPARAM>(&monitors));
    
    if (monitors.empty() || streamIndex >= monitors.size() || streamIndex < 0) {
        if (streamIndex != 0) {
            delete sess;
            return nullptr;
        }
    }
    
    HMONITOR targetMonitor = monitors.empty() ? nullptr : monitors[streamIndex].hMonitor;
    
    // Create GraphicsCaptureItem for monitor
    auto factory = winrt::get_activation_factory<GraphicsCaptureItem, IGraphicsCaptureItemInterop>();
    winrt::com_ptr<::IInspectable> itemInterop;
    HRESULT hr = factory->CreateForMonitor(targetMonitor, winrt::guid_of<ABI::Windows::Graphics::Capture::IGraphicsCaptureItem>(), itemInterop.put_void());
    if (FAILED(hr)) {
        delete sess;
        return nullptr;
    }
    
    sess->item = itemInterop.as<GraphicsCaptureItem>();
    sess->d3dDevice = CreateD3DDevice();
    sess->d3dDevice->GetImmediateContext(sess->d3dContext.put());
    sess->device = CreateDirect3DDevice(sess->d3dDevice.get());
    
    sess->framePool = Direct3D11CaptureFramePool::CreateFreeThreaded(
        sess->device,
        DirectXPixelFormat::B8G8R8A8UIntNormalized,
        1,
        sess->item.Size());
        
    sess->session = sess->framePool.CreateCaptureSession(sess->item);
    
    sess->framePool.FrameArrived([sess](auto const& pool, auto const&) {
        auto frame = pool.TryGetNextFrame();
        if (!frame) return;
        
        winrt::com_ptr<ID3D11Texture2D> surfaceTexture;
        auto surfaceUnknown = frame.Surface().template as<::IUnknown>();
        winrt::com_ptr<IDirect3DDxgiInterfaceAccess> surfaceInterop;
        constexpr GUID kDirect3DDxgiInterfaceAccessIID = {0xA9B3D012, 0x3DF2, 0x4EE3, {0xB8, 0xD1, 0x86, 0x95, 0xF4, 0x57, 0xD3, 0xC1}};
        if (FAILED(surfaceUnknown->QueryInterface(kDirect3DDxgiInterfaceAccessIID, surfaceInterop.put_void()))) {
            return;
        }
        surfaceInterop->GetInterface(winrt::guid_of<ID3D11Texture2D>(), surfaceTexture.put_void());
        
        D3D11_TEXTURE2D_DESC desc;
        surfaceTexture->GetDesc(&desc);
        
        // Create staging texture
        desc.Usage = D3D11_USAGE_STAGING;
        desc.BindFlags = 0;
        desc.CPUAccessFlags = D3D11_CPU_ACCESS_READ;
        desc.MiscFlags = 0;
        
        winrt::com_ptr<ID3D11Texture2D> stagingTexture;
        sess->d3dDevice->CreateTexture2D(&desc, nullptr, stagingTexture.put());
        
        sess->d3dContext->CopyResource(stagingTexture.get(), surfaceTexture.get());
        
        D3D11_MAPPED_SUBRESOURCE mapped;
        if (SUCCEEDED(sess->d3dContext->Map(stagingTexture.get(), 0, D3D11_MAP_READ, 0, &mapped))) {
            if (sess->videoCallback) {
                sess->videoCallback(sess->goID, mapped.pData, mapped.RowPitch * desc.Height, desc.Width, desc.Height);
            }
            sess->d3dContext->Unmap(stagingTexture.get(), 0);
        }
    });

    if (includeAudio) {
        winrt::com_ptr<IMMDeviceEnumerator> enumerator;
        CoCreateInstance(__uuidof(MMDeviceEnumerator), nullptr, CLSCTX_ALL, __uuidof(IMMDeviceEnumerator), enumerator.put_void());
        
        winrt::com_ptr<IMMDevice> renderDevice;
        enumerator->GetDefaultAudioEndpoint(eRender, eConsole, renderDevice.put());
        
        renderDevice->Activate(__uuidof(IAudioClient), CLSCTX_ALL, nullptr, sess->audioClient.put_void());
        
        WAVEFORMATEX* waveFormat = nullptr;
        sess->audioClient->GetMixFormat(&waveFormat);
        
        // Force 48kHz, 16-bit, stereo for standard output
        waveFormat->nSamplesPerSec = 48000;
        waveFormat->wBitsPerSample = 16;
        waveFormat->nChannels = 2;
        waveFormat->nBlockAlign = (waveFormat->nChannels * waveFormat->wBitsPerSample) / 8;
        waveFormat->nAvgBytesPerSec = waveFormat->nSamplesPerSec * waveFormat->nBlockAlign;
        
        REFERENCE_TIME hnsRequestedDuration = 10000000; // 1 second
        sess->audioClient->Initialize(AUDCLNT_SHAREMODE_SHARED, AUDCLNT_STREAMFLAGS_LOOPBACK | AUDCLNT_STREAMFLAGS_EVENTCALLBACK, hnsRequestedDuration, 0, waveFormat, nullptr);
        
        sess->audioEvent = CreateEvent(nullptr, FALSE, FALSE, nullptr);
        sess->audioClient->SetEventHandle(sess->audioEvent);
        sess->audioClient->GetService(__uuidof(IAudioCaptureClient), sess->captureClient.put_void());
        
        CoTaskMemFree(waveFormat);
    }
    
    return sess;
}

void StartWinCapture(void* ctx) {
    if (!ctx) return;
    WinCaptureSession* sess = static_cast<WinCaptureSession*>(ctx);
    sess->isRunning = true;
    sess->session.StartCapture();
    if (sess->includeAudio) {
        sess->audioClient->Start();
        sess->audioThread = std::thread(AudioCaptureLoop, sess);
    }
}

void StopWinCapture(void* ctx) {
    if (!ctx) return;
    WinCaptureSession* sess = static_cast<WinCaptureSession*>(ctx);
    sess->isRunning = false;
    sess->session.Close();
    sess->framePool.Close();
    if (sess->includeAudio) {
        sess->audioClient->Stop();
        SetEvent(sess->audioEvent);
        if (sess->audioThread.joinable()) {
            sess->audioThread.join();
        }
        CloseHandle(sess->audioEvent);
    }
}

void FreeWinCapture(void* ctx) {
    if (!ctx) return;
    WinCaptureSession* sess = static_cast<WinCaptureSession*>(ctx);
    delete sess;
}
