import java.util.Properties

plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
    id("rust")
}

val tauriProperties = Properties().apply {
    val propFile = file("tauri.properties")
    if (propFile.exists()) {
        propFile.inputStream().use { load(it) }
    }
}

val releaseSigningValues = mapOf(
    "storeFile" to System.getenv("BTC09_ANDROID_KEYSTORE")?.takeIf { it.isNotBlank() },
    "storePassword" to System.getenv("BTC09_ANDROID_KEYSTORE_PASSWORD")?.takeIf { it.isNotBlank() },
    "keyAlias" to System.getenv("BTC09_ANDROID_KEY_ALIAS")?.takeIf { it.isNotBlank() },
    "keyPassword" to System.getenv("BTC09_ANDROID_KEY_PASSWORD")?.takeIf { it.isNotBlank() },
)
val hasAnyReleaseSigningValue = releaseSigningValues.values.any { it != null }
val hasAllReleaseSigningValues = releaseSigningValues.values.all { it != null }
if (hasAnyReleaseSigningValue && !hasAllReleaseSigningValues) {
    throw GradleException(
        "Android release signing is incomplete. Set all four BTC09_ANDROID signing variables or none of them."
    )
}

android {
    compileSdk = 36
    ndkVersion = "29.0.14206865"
    namespace = "org.bitcoin09.wallet"
    defaultConfig {
        applicationId = "org.bitcoin09.wallet"
        minSdk = 24
        targetSdk = 36
        versionCode = tauriProperties.getProperty("tauri.android.versionCode", "1").toInt()
        versionName = tauriProperties.getProperty("tauri.android.versionName", "1.0")
    }
    signingConfigs {
        if (hasAllReleaseSigningValues) {
            create("release") {
                storeFile = rootProject.file(releaseSigningValues.getValue("storeFile")!!)
                storePassword = releaseSigningValues.getValue("storePassword")!!
                keyAlias = releaseSigningValues.getValue("keyAlias")!!
                keyPassword = releaseSigningValues.getValue("keyPassword")!!
            }
        }
    }
    buildTypes {
        getByName("debug") {
            isDebuggable = true
            isJniDebuggable = true
            isMinifyEnabled = false
            packaging {
                jniLibs.keepDebugSymbols.add("*/arm64-v8a/*.so")
                jniLibs.keepDebugSymbols.add("*/armeabi-v7a/*.so")
                jniLibs.keepDebugSymbols.add("*/x86/*.so")
                jniLibs.keepDebugSymbols.add("*/x86_64/*.so")
            }
        }
        getByName("release") {
            isMinifyEnabled = true
            if (hasAllReleaseSigningValues) {
                signingConfig = signingConfigs.getByName("release")
            }
            proguardFiles(
                *fileTree(".") { include("**/*.pro") }
                    .plus(getDefaultProguardFile("proguard-android-optimize.txt"))
                    .toList().toTypedArray()
            )
        }
    }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    buildFeatures {
        buildConfig = true
    }
}

kotlin {
    compilerOptions {
        jvmTarget.set(org.jetbrains.kotlin.gradle.dsl.JvmTarget.JVM_17)
    }
}

rust {
    rootDirRel = "../../../"
}

dependencies {
    implementation("androidx.webkit:webkit:1.14.0")
    implementation("androidx.appcompat:appcompat:1.7.1")
    implementation("androidx.activity:activity-ktx:1.10.1")
    implementation("com.google.android.material:material:1.12.0")
    implementation("androidx.lifecycle:lifecycle-process:2.10.0")
    testImplementation("junit:junit:4.13.2")
    androidTestImplementation("androidx.test.ext:junit:1.1.4")
    androidTestImplementation("androidx.test.espresso:espresso-core:3.5.0")
}

apply(from = "tauri.build.gradle.kts")
