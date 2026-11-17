package io.github.imilan

import android.Manifest
import android.annotation.SuppressLint
import android.app.Activity
import android.content.Intent
import android.content.pm.PackageManager
import android.content.res.Configuration
import android.graphics.Color
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.util.Log
import android.view.View
import android.view.ViewGroup
import android.view.WindowInsets
import android.view.WindowInsetsController
import android.webkit.CookieManager
import android.webkit.JavascriptInterface
import android.webkit.WebChromeClient
import android.webkit.WebResourceError
import android.webkit.WebResourceRequest
import android.webkit.WebSettings
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.Button
import android.widget.FrameLayout
import android.widget.ProgressBar
import android.widget.TextView

class MainActivity : Activity() {
    private lateinit var root: FrameLayout
    private lateinit var web: WebView
    private lateinit var overlay: View
    private lateinit var progress: ProgressBar
    private lateinit var status: TextView
    private lateinit var retry: Button
    private lateinit var chrome: ImiChromeClient

    private val handler = Handler(Looper.getMainLooper())
    private var loaded = false
    private var customView: View? = null
    private var customCallback: WebChromeClient.CustomViewCallback? = null
    private var immersive = false

    private val pollState = object : Runnable {
        override fun run() {
            when (Engine.state) {
                Engine.State.READY -> showWeb()
                Engine.State.FAILED -> showError(Engine.message)
                else -> handler.postDelayed(this, 250)
            }
        }
    }

    @SuppressLint("SetJavaScriptEnabled")
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_main)

        root = findViewById(R.id.root)
        web = findViewById(R.id.web)
        overlay = findViewById(R.id.overlay)
        progress = findViewById(R.id.progress)
        status = findViewById(R.id.status)
        retry = findViewById(R.id.retry)

        window.addFlags(android.view.WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON)
        setupEdgeToEdge()
        applySystemBarInsets()
        applyDarkChrome(isNightMode())
        askNotificationPermission()

        setupWebView()
        retry.setOnClickListener {
            progress.visibility = View.VISIBLE
            retry.visibility = View.GONE
            status.text = getString(R.string.starting)
            EngineService.restart(this)
            handler.removeCallbacks(pollState)
            handler.post(pollState)
        }

        EngineService.start(this)
        handler.post(pollState)
    }

    /**
     * targetSdk 35 默认强制 edge-to-edge，网页会画到状态栏/导航栏下面。
     * 统一按 edge-to-edge 布局，由下面的 insets 监听补内边距；进入全屏时 inset 变 0，自动还原。
     */
    private fun setupEdgeToEdge() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
            window.setDecorFitsSystemWindows(false)
        } else {
            @Suppress("DEPRECATION")
            window.decorView.systemUiVisibility = (
                View.SYSTEM_UI_FLAG_LAYOUT_STABLE
                    or View.SYSTEM_UI_FLAG_LAYOUT_FULLSCREEN
                    or View.SYSTEM_UI_FLAG_LAYOUT_HIDE_NAVIGATION
                )
        }
    }

    /** 给根布局补系统栏与输入法内边距；全屏隐藏系统栏时 inset 为 0，网页自然铺满。 */
    private fun applySystemBarInsets() {
        val view = findViewById<View>(R.id.root)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
            view.setOnApplyWindowInsetsListener { v, insets ->
                val bars = insets.getInsets(
                    WindowInsets.Type.systemBars()
                        or WindowInsets.Type.displayCutout()
                        or WindowInsets.Type.ime()
                )
                v.setPadding(bars.left, bars.top, bars.right, bars.bottom)
                insets
            }
        } else {
            @Suppress("DEPRECATION")
            view.setOnApplyWindowInsetsListener { v, insets ->
                v.setPadding(
                    insets.systemWindowInsetLeft,
                    insets.systemWindowInsetTop,
                    insets.systemWindowInsetRight,
                    insets.systemWindowInsetBottom,
                )
                insets
            }
        }
    }

    private fun isNightMode(): Boolean =
        (resources.configuration.uiMode and Configuration.UI_MODE_NIGHT_MASK) ==
            Configuration.UI_MODE_NIGHT_YES

    /**
     * manifest 里把 uiMode 交给 Activity 自己处理，切系统深浅色时页面不会重建。
     * 这里手动同步三件事：壳的底色、系统栏图标的明暗、网页的「自动」模式，
     * 否则要杀进程重开才跟得上。
     */
    override fun onConfigurationChanged(newConfig: Configuration) {
        super.onConfigurationChanged(newConfig)
        val night = (newConfig.uiMode and Configuration.UI_MODE_NIGHT_MASK) ==
            Configuration.UI_MODE_NIGHT_YES
        applyDarkChrome(night)
        if (::web.isInitialized) {
            web.setBackgroundColor(Color.parseColor(if (night) "#0E1726" else "#EDF2F7"))
            // WebView 的 prefers-color-scheme 在「自己处理 uiMode」时不一定跟着变，
            // 所以把壳的判定直接推给网页，网页拿这个值时会优先于 matchMedia。
            web.evaluateJavascript(
                "window.__imiSetSystemDark && window.__imiSetSystemDark(" +
                    (if (night) "true" else "false") + ");" +
                    "window.__imiResyncTheme && window.__imiResyncTheme()",
                null,
            )
        }
    }

    /**
     * 深色底要浅色图标、浅色底要深色图标。Activity 不重建，主题里的
     * windowLightStatusBar 不会重新解析，只能在这里手动翻。
     */
    @Suppress("DEPRECATION")
    private fun applyDarkChrome(night: Boolean) {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
            val light = WindowInsetsController.APPEARANCE_LIGHT_STATUS_BARS or
                WindowInsetsController.APPEARANCE_LIGHT_NAVIGATION_BARS
            window.insetsController?.setSystemBarsAppearance(if (night) 0 else light, light)
            return
        }
        val lightFlag = View.SYSTEM_UI_FLAG_LIGHT_STATUS_BAR
        var flags = window.decorView.systemUiVisibility
        flags = if (night) flags and lightFlag.inv() else flags or lightFlag
        window.decorView.systemUiVisibility = flags
    }

    private fun setupWebView() {
        WebView.setWebContentsDebuggingEnabled(BuildConfig.DEBUG)
        // 底色跟着当前主题走，否则浅色模式下网页加载瞬间会闪一块深蓝
        web.setBackgroundColor(Color.parseColor(if (isNightMode()) "#0E1726" else "#EDF2F7"))
        web.settings.apply {
            javaScriptEnabled = true
            domStorageEnabled = true
            databaseEnabled = true
            mediaPlaybackRequiresUserGesture = false
            cacheMode = WebSettings.LOAD_DEFAULT
            mixedContentMode = WebSettings.MIXED_CONTENT_ALWAYS_ALLOW
            allowFileAccess = false
            allowContentAccess = false
            // 禁止 WebView 页面级缩放；视频画面的捏合缩放由网页在「放大镜」开启时自行处理
            setSupportZoom(false)
            builtInZoomControls = false
            displayZoomControls = false
            useWideViewPort = true
            loadWithOverviewMode = true
            userAgentString = userAgentString + " IMI-LAN-Android/" + BuildConfig.VERSION_NAME
        }

        web.addJavascriptInterface(DownloadBridge(this), "IMIAndroid")
        web.addJavascriptInterface(ShellBridge(), "IMIShell")

        web.webViewClient = object : WebViewClient() {
            override fun shouldOverrideUrlLoading(view: WebView, request: WebResourceRequest): Boolean {
                val host = request.url.host ?: return false
                if (host == "127.0.0.1" || host == "localhost") return false
                try {
                    startActivity(Intent(Intent.ACTION_VIEW, request.url))
                } catch (_: Throwable) {
                }
                return true
            }

            override fun onPageFinished(view: WebView, url: String) {
                overlay.visibility = View.GONE
                web.visibility = View.VISIBLE
            }

            override fun onReceivedError(view: WebView, request: WebResourceRequest, error: WebResourceError) {
                if (!request.isForMainFrame) return
                if (Engine.state == Engine.State.READY || Engine.state == Engine.State.STARTING) return
                showError(error.description?.toString() ?: "页面加载失败")
            }
        }

        chrome = ImiChromeClient()
        web.webChromeClient = chrome
    }

    private fun showWeb() {
        if (loaded) return
        loaded = true
        val cookie = CookieManager.getInstance()
        cookie.setAcceptCookie(true)
        cookie.setCookie(
            "http://127.0.0.1:${Engine.webPort}/",
            "imi_app=${Engine.token}; Path=/; HttpOnly; SameSite=Lax",
        )
        cookie.flush()
        web.loadUrl(Engine.webUrl())
        progress.visibility = View.GONE
        retry.visibility = View.GONE
        status.text = ""
    }

    private fun showError(text: String) {
        handler.removeCallbacks(pollState)
        overlay.visibility = View.VISIBLE
        web.visibility = View.GONE
        progress.visibility = View.GONE
        retry.visibility = View.VISIBLE
        status.text = text.ifBlank { getString(R.string.engine_failed) }
        Log.w("IMI-Main", "engine error: $text")
    }

    private fun askNotificationPermission() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.TIRAMISU) return
        if (checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) == PackageManager.PERMISSION_GRANTED) return
        requestPermissions(arrayOf(Manifest.permission.POST_NOTIFICATIONS), 100)
    }

    override fun onResume() {
        super.onResume()
        if (loaded && Engine.state == Engine.State.READY) {
            // 从后台回来时让网页自己恢复（网页内有“后台 2 分钟休眠”逻辑）
            web.onResume()
            web.resumeTimers()
            if (!web.url.isNullOrEmpty()) web.evaluateJavascript("window.dispatchEvent(new Event('imi-resume'))", null)
        }
    }

    override fun onPause() {
        web.onPause()
        super.onPause()
    }

    override fun onDestroy() {
        handler.removeCallbacks(pollState)
        web.destroy()
        super.onDestroy()
    }

    @Deprecated("Deprecated in Java")
    override fun onBackPressed() {
        if (customView != null) {
            web.evaluateJavascript("document.fullscreenElement && document.exitFullscreen()", null)
            chrome.onHideCustomView()
            return
        }
        // 先让网页处理：退出全屏 / 大屏 / 弹窗；没处理再走 WebView 返回或退到后台
        web.evaluateJavascript("(window.__imiBack && window.__imiBack()) === true") { result ->
            if (result == "true") return@evaluateJavascript
            if (web.canGoBack()) web.goBack() else moveTaskToBack(true)
        }
    }

    override fun onWindowFocusChanged(hasFocus: Boolean) {
        super.onWindowFocusChanged(hasFocus)
        // 从通知栏/多任务回来后系统栏会重新出现，全屏状态下再隐藏一次
        if (hasFocus && immersive) hideSystemBars()
    }

    private fun hideSystemBars() {
        immersive = true
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
            window.insetsController?.let {
                it.systemBarsBehavior = WindowInsetsController.BEHAVIOR_SHOW_TRANSIENT_BARS_BY_SWIPE
                it.hide(WindowInsets.Type.systemBars())
            }
        } else {
            @Suppress("DEPRECATION")
            window.decorView.systemUiVisibility = (
                View.SYSTEM_UI_FLAG_IMMERSIVE_STICKY
                    or View.SYSTEM_UI_FLAG_FULLSCREEN
                    or View.SYSTEM_UI_FLAG_HIDE_NAVIGATION
                    or View.SYSTEM_UI_FLAG_LAYOUT_STABLE
                    or View.SYSTEM_UI_FLAG_LAYOUT_FULLSCREEN
                    or View.SYSTEM_UI_FLAG_LAYOUT_HIDE_NAVIGATION
                )
        }
    }

    private fun showSystemBars() {
        immersive = false
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
            window.insetsController?.show(WindowInsets.Type.systemBars())
        } else {
            @Suppress("DEPRECATION")
            window.decorView.systemUiVisibility = (
                View.SYSTEM_UI_FLAG_LAYOUT_STABLE
                    or View.SYSTEM_UI_FLAG_LAYOUT_FULLSCREEN
                    or View.SYSTEM_UI_FLAG_LAYOUT_HIDE_NAVIGATION
                )
        }
    }

    /** 网页全屏（大屏墙 / 单路全屏）时同步隐藏安卓系统栏。 */
    private inner class ShellBridge {
        @JavascriptInterface
        fun setFullscreen(on: Boolean) {
            runOnUiThread {
                if (on) hideSystemBars() else showSystemBars()
            }
        }

        /**
         * 网页「外观 → 自动」直接问壳要系统深浅色。configChanges 接管 uiMode 之后
         * WebView 的 prefers-color-scheme 可能一直停在旧值，靠它判断会看着像没生效。
         */
        @JavascriptInterface
        fun isNightMode(): Boolean = this@MainActivity.isNightMode()
    }

    inner class ImiChromeClient : WebChromeClient() {
        override fun onShowCustomView(view: View, callback: CustomViewCallback) {
            if (customView != null) {
                callback.onCustomViewHidden()
                return
            }
            customView = view
            customCallback = callback
            (window.decorView as FrameLayout).addView(
                view,
                FrameLayout.LayoutParams(
                    ViewGroup.LayoutParams.MATCH_PARENT,
                    ViewGroup.LayoutParams.MATCH_PARENT,
                ),
            )
            web.visibility = View.GONE
            hideSystemBars()
        }

        override fun onHideCustomView() {
            val view = customView ?: return
            (window.decorView as FrameLayout).removeView(view)
            customView = null
            customCallback?.onCustomViewHidden()
            customCallback = null
            web.visibility = View.VISIBLE
            // 大屏墙可能还开着，让网页决定是否继续隐藏系统栏
            web.evaluateJavascript(
                "(function(){var w=document.body.classList.contains('wall')||!!document.fullscreenElement;" +
                    "window.IMIShell&&window.IMIShell.setFullscreen(w)})()",
                null,
            )
        }
    }
}
