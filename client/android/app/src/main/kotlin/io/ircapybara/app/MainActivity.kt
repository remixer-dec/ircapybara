package io.ircapybara.app

import android.Manifest
import android.annotation.SuppressLint
import android.app.Activity
import android.content.ActivityNotFoundException
import android.content.ClipData
import android.content.ClipboardManager
import android.content.Intent
import android.content.pm.PackageManager
import android.graphics.Color
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.view.HapticFeedbackConstants
import android.view.View
import android.view.ViewGroup
import android.view.WindowInsets
import android.view.WindowInsetsController
import android.webkit.JavascriptInterface
import android.webkit.WebResourceRequest
import android.webkit.WebResourceResponse
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.FrameLayout
import android.window.OnBackInvokedCallback
import android.window.OnBackInvokedDispatcher
import org.json.JSONObject
import java.io.ByteArrayInputStream
import java.io.IOException
import java.util.concurrent.CompletableFuture

/**
 * A single web view showing the shared frontend from the APK's assets.
 *
 * The page talks to the Go core through [WebBridge] (page to native) and
 * `window.__ircapybara` (native to page); see frontend/dist/bridge.js for the
 * other side of both.
 */
class MainActivity : Activity() {
    private lateinit var app: IRCapybaraApp
    private lateinit var root: FrameLayout
    private lateinit var webView: WebView

    private var pendingDocument: CompletableFuture<Uri?>? = null
    private var pendingPermission: CompletableFuture<Boolean>? = null
    private var backCallback: OnBackInvokedCallback? = null

    @SuppressLint("SetJavaScriptEnabled")
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        app = application as IRCapybaraApp

        // Draw behind the system bars and pad the content by the real insets.
        // Including the keyboard inset makes the web view shrink when the
        // keyboard opens, which the page sees as an ordinary resize.
        window.setDecorFitsSystemWindows(false)
        root = FrameLayout(this)
        root.setOnApplyWindowInsetsListener { view, insets ->
            val bars = insets.getInsets(
                WindowInsets.Type.systemBars() or WindowInsets.Type.displayCutout() or WindowInsets.Type.ime(),
            )
            view.setPadding(bars.left, bars.top, bars.right, bars.bottom)
            WindowInsets.CONSUMED
        }

        webView = WebView(this)
        webView.settings.apply {
            javaScriptEnabled = true
            domStorageEnabled = true
            // The page is served through shouldInterceptRequest, not file URLs.
            allowFileAccess = false
            allowContentAccess = false
            // Lets the message sound play for a message that arrives without a tap.
            mediaPlaybackRequiresUserGesture = false
            setSupportZoom(false)
        }
        webView.setBackgroundColor(Color.TRANSPARENT)
        webView.overScrollMode = View.OVER_SCROLL_NEVER
        webView.isVerticalScrollBarEnabled = false
        webView.webViewClient = AssetClient()
        webView.addJavascriptInterface(WebBridge(), "IRCapybaraAndroid")
        root.addView(webView, ViewGroup.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT))
        setContentView(root)
        applyChrome(dark = false)

        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            val callback = OnBackInvokedCallback { handleBack() }
            onBackInvokedDispatcher.registerOnBackInvokedCallback(OnBackInvokedDispatcher.PRIORITY_DEFAULT, callback)
            backCallback = callback
        }

        app.host.activity = this
        webView.loadUrl("https://$ASSET_HOST/index.html")
    }

    override fun onResume() {
        super.onResume()
        app.host.clearMessageNotifications()
        // The device may have slept or changed networks while we were away.
        app.probe()
    }

    override fun onDestroy() {
        if (app.host.activity === this) app.host.activity = null
        pendingDocument?.complete(null)
        pendingPermission?.complete(false)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            backCallback?.let { onBackInvokedDispatcher.unregisterOnBackInvokedCallback(it) }
        }
        root.removeView(webView)
        webView.destroy()
        super.onDestroy()
    }

    @Deprecated("Used on Android 11 and 12, which have no OnBackInvokedDispatcher.")
    override fun onBackPressed() {
        handleBack()
    }

    /**
     * The page gets the first say: it closes a sheet or returns to the chat
     * list. With nothing left to close the app goes to the background instead
     * of finishing, so the page stays loaded and can still raise notifications.
     */
    private fun handleBack() {
        webView.evaluateJavascript("!!(window.__ircapybara&&window.__ircapybara.back())") { handled ->
            if (handled != "true") moveTaskToBack(true)
        }
    }

    /** Delivers a core event to the page. Runs on the main thread. */
    fun dispatchEvent(event: String, payloadJson: String) {
        webView.evaluateJavascript(
            "window.__ircapybara&&window.__ircapybara.dispatch(${JSONObject.quote(event)},$payloadJson)",
            null,
        )
    }

    /** Opens the system document picker and blocks the calling (non-main) thread for the result. */
    fun pickDocument(): Uri? {
        val result = CompletableFuture<Uri?>()
        runOnUiThread {
            pendingDocument?.complete(null)
            pendingDocument = result
            try {
                startActivityForResult(
                    Intent(Intent.ACTION_OPEN_DOCUMENT).addCategory(Intent.CATEGORY_OPENABLE).setType("*/*"),
                    REQUEST_DOCUMENT,
                )
            } catch (_: ActivityNotFoundException) {
                pendingDocument = null
                result.complete(null)
            }
        }
        return result.get()
    }

    @Deprecated("The framework Activity has no result-launcher API.")
    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        super.onActivityResult(requestCode, resultCode, data)
        if (requestCode != REQUEST_DOCUMENT) return
        pendingDocument?.complete(if (resultCode == RESULT_OK) data?.data else null)
        pendingDocument = null
    }

    /** Asks for the notification permission and blocks the calling (non-main) thread for the answer. */
    fun requestNotificationPermission(): Boolean {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.TIRAMISU) return true
        val result = CompletableFuture<Boolean>()
        runOnUiThread {
            pendingPermission?.complete(false)
            pendingPermission = result
            requestPermissions(arrayOf(Manifest.permission.POST_NOTIFICATIONS), REQUEST_NOTIFICATIONS)
        }
        return result.get()
    }

    override fun onRequestPermissionsResult(requestCode: Int, permissions: Array<out String>, grantResults: IntArray) {
        super.onRequestPermissionsResult(requestCode, permissions, grantResults)
        if (requestCode != REQUEST_NOTIFICATIONS) return
        pendingPermission?.complete(grantResults.firstOrNull() == PackageManager.PERMISSION_GRANTED)
        pendingPermission = null
    }

    /** Colours the area behind the system bars to match the page's theme. */
    private fun applyChrome(dark: Boolean) {
        root.setBackgroundColor(getColor(if (dark) R.color.chrome_dark else R.color.chrome_light))
        val light = WindowInsetsController.APPEARANCE_LIGHT_STATUS_BARS or
            WindowInsetsController.APPEARANCE_LIGHT_NAVIGATION_BARS
        window.insetsController?.setSystemBarsAppearance(if (dark) 0 else light, light)
    }

    private fun openExternal(url: String) {
        val uri = Uri.parse(url)
        if (uri.scheme != "http" && uri.scheme != "https") return
        try {
            startActivity(Intent(Intent.ACTION_VIEW, uri))
        } catch (_: ActivityNotFoundException) {
            // No browser installed; nothing useful to do.
        }
    }

    /** The object the page sees as `window.IRCapybaraAndroid`. Its methods run on a web view thread. */
    inner class WebBridge {
        /** Calls a Go frontend-API method and settles the page's promise with the outcome. */
        @JavascriptInterface
        fun invoke(id: Int, method: String, argsJson: String) {
            app.calls.execute {
                var ok = true
                val payload = try {
                    app.bridge.invoke(method, argsJson)
                } catch (e: Exception) {
                    ok = false
                    JSONObject.quote(e.message ?: "Something went wrong.")
                }
                runOnUiThread {
                    webView.evaluateJavascript("window.__ircapybara&&window.__ircapybara.settle($id,$ok,$payload)", null)
                }
            }
        }

        @JavascriptInterface
        fun openExternal(url: String) {
            runOnUiThread { this@MainActivity.openExternal(url) }
        }

        @JavascriptInterface
        fun setChrome(dark: Boolean) {
            runOnUiThread { applyChrome(dark) }
        }

        @JavascriptInterface
        fun copyText(text: String) {
            runOnUiThread {
                getSystemService(ClipboardManager::class.java)
                    .setPrimaryClip(ClipData.newPlainText(getString(R.string.app_name), text))
            }
        }

        @JavascriptInterface
        fun haptic(kind: String) {
            runOnUiThread {
                webView.performHapticFeedback(
                    if (kind == "longpress") HapticFeedbackConstants.LONG_PRESS else HapticFeedbackConstants.KEYBOARD_TAP,
                )
            }
        }
    }

    /**
     * Serves the packaged frontend under a fixed https origin and nothing else.
     * An https origin (rather than file://) gives the page a normal, secure
     * origin for storage and audio. Every other request is refused, so content
     * received over IRC can never make the web view talk to the network.
     */
    private inner class AssetClient : WebViewClient() {
        override fun shouldInterceptRequest(view: WebView, request: WebResourceRequest): WebResourceResponse {
            val url = request.url
            if (url.scheme != "https" || url.host != ASSET_HOST) return refuse(403, "Forbidden")
            val path = url.path.orEmpty().trimStart('/').ifEmpty { "index.html" }
            if (path.split('/').any { it.isEmpty() || it == "." || it == ".." }) return refuse(404, "Not Found")
            return try {
                val type = mimeType(path)
                val text = type.startsWith("text/") || type == "image/svg+xml" || type == "application/json"
                WebResourceResponse(type, if (text) "utf-8" else null, assets.open("$ASSET_ROOT/$path"))
            } catch (_: IOException) {
                refuse(404, "Not Found")
            }
        }

        override fun shouldOverrideUrlLoading(view: WebView, request: WebResourceRequest): Boolean {
            if (request.url.host == ASSET_HOST) return false
            // Links are opened by the page through the bridge; anything that
            // still tries to navigate the web view leaves the app instead.
            openExternal(request.url.toString())
            return true
        }

        private fun refuse(status: Int, reason: String) =
            WebResourceResponse("text/plain", "utf-8", status, reason, emptyMap(), ByteArrayInputStream(ByteArray(0)))

        private fun mimeType(path: String): String = when (path.substringAfterLast('.', "").lowercase()) {
            "html" -> "text/html"
            "js" -> "text/javascript"
            "css" -> "text/css"
            "svg" -> "image/svg+xml"
            "png" -> "image/png"
            "wav" -> "audio/wav"
            "json" -> "application/json"
            else -> "application/octet-stream"
        }
    }

    companion object {
        // Reserved by Google for serving app assets to a web view; it never resolves on the network.
        private const val ASSET_HOST = "appassets.androidplatform.net"
        private const val ASSET_ROOT = "dist"
        private const val REQUEST_DOCUMENT = 1
        private const val REQUEST_NOTIFICATIONS = 2
    }
}
