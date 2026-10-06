// ffi.kt

package ffi

import android.content.Context
import java.nio.ByteBuffer

import com.hex.p2pdesk.RdClipboardManager

object FFI {
    init {
        System.loadLibrary("rustdesk")
        // The go-libp2p network module: load it first so the Rust side can
        // dlopen it by name (Android dlopen matches already-loaded libraries
        // by soname without a path).
        System.loadLibrary("p2pdesk_net")
    }

    external fun init(ctx: Context)
    external fun onAppStart(ctx: Context)
    external fun setClipboardManager(clipboardManager: RdClipboardManager)
    external fun startServer(app_dir: String, custom_client_config: String)
    external fun startService()
    external fun onVideoFrameUpdate(buf: ByteBuffer)
    external fun onAudioFrameUpdate(buf: ByteBuffer)
    external fun translateLocale(localeName: String, input: String): String
    external fun refreshScreen()
    external fun setFrameRawEnable(name: String, value: Boolean)
    external fun setCodecInfo(info: String)
    external fun getLocalOption(key: String): String
    external fun getBuildinOption(key: String): String
    external fun onClipboardUpdate(clips: ByteBuffer)
    external fun isServiceClipboardEnabled(): Boolean
}
