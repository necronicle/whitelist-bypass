package bypass.whitelist

import android.util.Log
import android.annotation.SuppressLint
import android.content.ClipData
import android.content.ClipboardManager
import android.content.Intent
import android.net.Uri
import android.net.VpnService
import android.os.Bundle
import android.webkit.*
import android.widget.Button
import android.widget.EditText
import android.widget.ImageButton
import android.widget.TextView
import android.widget.Toast
import androidx.activity.enableEdgeToEdge
import androidx.activity.result.contract.ActivityResultContracts
import androidx.appcompat.app.AppCompatActivity
import androidx.core.view.ViewCompat
import androidx.core.view.WindowInsetsCompat
import androidx.core.content.ContextCompat
import mobile.Mobile
import mobile.LogCallback

class MainActivity : AppCompatActivity() {

    private lateinit var webView: WebView
    private lateinit var logView: TextView
    private lateinit var urlInput: EditText
    private var relayStarted = false
    private var vpnRequested = false

    private val vpnLauncher = registerForActivityResult(
        ActivityResultContracts.StartActivityForResult()
    ) { result ->
        if (result.resultCode == RESULT_OK) {
            startVpnService()
        } else {
            vpnRequested = false
            appendLog("VPN permission denied")
        }
    }

    private val hookVk by lazy {
        assets.open("joiner-vk.js").bufferedReader().readText()
    }

    private val hookTelemost by lazy {
        assets.open("joiner-telemost.js").bufferedReader().readText()
    }

    private fun hookForUrl(url: String): String {
        return if (url.contains("telemost.yandex")) hookTelemost else hookVk
    }

    @SuppressLint("SetJavaScriptEnabled")
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        setContentView(R.layout.activity_main)
        ViewCompat.setOnApplyWindowInsetsListener(findViewById(R.id.main)) { v, insets ->
            val systemBars = insets.getInsets(WindowInsetsCompat.Type.systemBars())
            v.setPadding(systemBars.left, systemBars.top, systemBars.right, systemBars.bottom)
            insets
        }

        logView = findViewById(R.id.logView)
        urlInput = findViewById(R.id.urlInput)
        webView = findViewById(R.id.webView)

        setupWebView()
        startRelay()

        val goButton = findViewById<Button>(R.id.goButton)
        goButton.setOnClickListener {
            val rawUrl = urlInput.text.toString().trim()
            val url = normalizeCallUrl(rawUrl)
            if (url == null) {
                appendLog("Unsupported call link: $rawUrl")
                Toast.makeText(this, "Use a VK Call or Telemost link", Toast.LENGTH_SHORT).show()
                return@setOnClickListener
            }

            if (url != rawUrl) {
                urlInput.setText(url)
            }

            appendLog("Loading: $url")
            webView.loadUrl(url)
        }

        findViewById<ImageButton>(R.id.copyLogsButton).setOnClickListener {
            val clip = ClipData.newPlainText("logs", logView.text)
            (getSystemService(CLIPBOARD_SERVICE) as ClipboardManager).setPrimaryClip(clip)
            Toast.makeText(this, "Logs copied", Toast.LENGTH_SHORT).show()
        }

        if (CALL_LINK.isNotEmpty()) {
            urlInput.setText(CALL_LINK)
            goButton.performClick()
        }
    }

    override fun onResume() {
        super.onResume()
        if (!TunnelVpnService.isRunning) {
            vpnRequested = false
        }
    }

    private fun updateVpnStatus(status: VpnStatus) {
        TunnelVpnService.instance?.updateStatus(status)
    }

    private fun startRelay() {
        if (relayStarted) return
        relayStarted = true
        val cb = LogCallback { msg ->
            appendLog(msg)
            if (msg.contains("browser connected")) updateVpnStatus(VpnStatus.TUNNEL_ACTIVE)
            else if (msg.contains("ws read error")) updateVpnStatus(VpnStatus.TUNNEL_LOST)
        }
        Thread {
            try {
                Mobile.startJoiner(9000, 1080, cb)
            } catch (e: Exception) {
                relayStarted = false
                appendLog("Relay error: ${e.message}")
            }
        }.start()
        appendLog("Relay started (SOCKS5 :1080, WS :9000)")
    }

    @SuppressLint("SetJavaScriptEnabled")
    private fun setupWebView() {
        webView.settings.apply {
            javaScriptEnabled = true
            domStorageEnabled = true
            mediaPlaybackRequiresUserGesture = false
            allowContentAccess = false
            allowFileAccess = false
            setSupportMultipleWindows(false)
            useWideViewPort = true
            loadWithOverviewMode = true
            builtInZoomControls = true
            displayZoomControls = false
            userAgentString = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
        }

        webView.addJavascriptInterface(JsBridge(), "AndroidBridge")

        webView.webChromeClient = object : WebChromeClient() {
            override fun onPermissionRequest(request: PermissionRequest) {
                runOnUiThread { request.grant(request.resources) }
            }

            override fun onConsoleMessage(msg: ConsoleMessage): Boolean {
                val text = msg.message()
                Log.d("HOOK", text)
                if (text.contains("[HOOK]")) {
                    appendLog(text)
                    if (text.contains("CALL CONNECTED")) updateVpnStatus(VpnStatus.CALL_CONNECTED)
                    else if (text.contains("DataChannel open")) updateVpnStatus(VpnStatus.DATACHANNEL_OPEN)
                    else if (text.contains("DataChannel closed")) updateVpnStatus(VpnStatus.DATACHANNEL_LOST)
                    else if (text.contains("WebSocket connected")) updateVpnStatus(VpnStatus.TUNNEL_ACTIVE)
                    else if (text.contains("WebSocket disconnected")) updateVpnStatus(VpnStatus.TUNNEL_LOST)
                    else if (text.contains("Connection state: connecting")) updateVpnStatus(VpnStatus.CONNECTING)
                    else if (text.contains("Connection state: disconnected")) updateVpnStatus(VpnStatus.CALL_DISCONNECTED)
                    else if (text.contains("Connection state: failed")) updateVpnStatus(VpnStatus.CALL_FAILED)
                }
                return true
            }
        }

        webView.webViewClient = object : WebViewClient() {
            override fun shouldInterceptRequest(view: WebView, request: WebResourceRequest): WebResourceResponse? {
                val url = request.url.toString()
                if (!url.contains("telemost.yandex.ru/j/")) return null
                if (request.method != "GET") return null
                try {
                    val conn = java.net.URL(url).openConnection() as java.net.HttpURLConnection
                    conn.requestMethod = "GET"
                    request.requestHeaders?.forEach { (k, v) -> conn.setRequestProperty(k, v) }
                    val headers = mutableMapOf<String, String>()
                    conn.headerFields?.forEach { (k, v) ->
                        if (k != null && !k.equals("content-security-policy", ignoreCase = true)
                            && !k.equals("content-security-policy-report-only", ignoreCase = true)) {
                            headers[k] = v.joinToString(", ")
                        }
                    }
                    return WebResourceResponse(
                        conn.contentType?.split(";")?.firstOrNull() ?: "text/html",
                        "utf-8",
                        conn.responseCode,
                        conn.responseMessage ?: "OK",
                        headers,
                        conn.inputStream
                    )
                } catch (e: Exception) {
                    return null
                }
            }
            override fun onPageStarted(view: WebView, url: String, favicon: android.graphics.Bitmap?) {
                view.evaluateJavascript("""(function(){
var oac=window.AudioContext||window.webkitAudioContext;
if(oac){var nac=function(){var c=new oac();c.suspend();
  c.resume=function(){return Promise.resolve()};
  return c};
  nac.prototype=oac.prototype;window.AudioContext=nac;
  if(window.webkitAudioContext)window.webkitAudioContext=nac}
})()""", null)
            }
            override fun onPageFinished(view: WebView, url: String) {
                val hook = hookForUrl(url)
                appendLog("Page loaded, injecting hook for $url")
                view.evaluateJavascript(hook, null)
            }
        }
    }

    private fun appendLog(msg: String) {
        runOnUiThread {
            val clean = msg.replace("[HOOK] ", "")
            val current = logView.text.toString()
            val trimmed = if (current.length > 30000) current.takeLast(20000) else current
            logView.text = trimmed + clean + "\n"
            val scrollAmount = logView.layout?.let {
                it.getLineTop(logView.lineCount) - logView.height
            } ?: 0
            if (scrollAmount > 0) logView.scrollTo(0, scrollAmount)
        }
    }

    private fun requestVpn() {
        if (TunnelVpnService.isRunning || vpnRequested) {
            return
        }
        vpnRequested = true
        val intent = VpnService.prepare(this)
        if (intent != null) {
            vpnLauncher.launch(intent)
        } else {
            startVpnService()
        }
    }

    private fun startVpnService() {
        if (TunnelVpnService.isRunning) {
            appendLog("VPN already running")
            return
        }
        val intent = Intent(this, TunnelVpnService::class.java)
        ContextCompat.startForegroundService(this, intent)
        appendLog("VPN started")
        updateVpnStatus(VpnStatus.TUNNEL_ACTIVE)
    }

    private fun normalizeCallUrl(rawUrl: String): String? {
        if (rawUrl.isBlank()) return null

        val candidate = if ("://" in rawUrl) rawUrl else "https://$rawUrl"
        val uri = Uri.parse(candidate)
        val scheme = uri.scheme?.lowercase() ?: return null
        val host = uri.host?.lowercase() ?: return null
        if (scheme != "http" && scheme != "https") return null

        return when {
            host == "vk.com" || host.endsWith(".vk.com") -> candidate
            host == "telemost.yandex.ru" || host.endsWith(".telemost.yandex.ru") -> candidate
            else -> null
        }
    }


    @Suppress("unused")
    inner class JsBridge {
        @JavascriptInterface
        fun log(msg: String) {
            appendLog(msg)
        }

        @JavascriptInterface
        fun onTunnelReady() {
            appendLog("Tunnel ready, starting VPN...")
            updateVpnStatus(VpnStatus.TUNNEL_ACTIVE)
            runOnUiThread { requestVpn() }
        }
    }

    companion object {
        private const val CALL_LINK = "" // Open call page on app start
    }
}
