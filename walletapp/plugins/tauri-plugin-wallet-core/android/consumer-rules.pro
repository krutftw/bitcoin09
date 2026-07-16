# The Kotlin bridge calls the generated Go API directly, but keep the complete
# binding surface so R8 cannot remove JNI entry points that gomobile resolves.
-keep class org.bitcoin09.mobilewallet.** { *; }
-keep class go.** { *; }
