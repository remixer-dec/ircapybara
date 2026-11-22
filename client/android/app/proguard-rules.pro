# gomobile uses these generated classes, members, and signatures from JNI.
-keep class go.** { *; }
-keep class io.ircapybara.mobile.** { *; }

# WebView looks up the annotated bridge methods by their JavaScript names.
-keepattributes RuntimeVisibleAnnotations
-keepclassmembers class io.ircapybara.app.MainActivity$WebBridge {
    @android.webkit.JavascriptInterface <methods>;
}
