import org.jetbrains.kotlin.gradle.dsl.JvmTarget

plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

android {
    namespace = "io.ircapybara.app"
    compileSdk = 36

    defaultConfig {
        applicationId = "io.ircapybara.app"
        minSdk = 24
        targetSdk = 35
        versionCode = 1
        versionName = "0.1.0"
    }

    buildTypes {
        release {
            ndk.abiFilters.addAll(listOf("armeabi-v7a", "arm64-v8a"))
            isMinifyEnabled = true
            isShrinkResources = true
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"), "proguard-rules.pro")
        }
    }

    packaging {
        jniLibs.useLegacyPackaging = true
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    // Package the desktop frontend under assets/dist for the fixed WebView origin.
    sourceSets["main"].assets.srcDir(rootProject.file("../frontend"))
    sourceSets["main"].assets.exclude("dist/assets/capybara/README.md")
}

kotlin {
    compilerOptions {
        jvmTarget.set(JvmTarget.JVM_17)
    }
}

val coreLibrary = layout.projectDirectory.file("libs/ircapybara.aar")

tasks.named("preBuild") {
    doFirst {
        if (!coreLibrary.asFile.exists()) {
            throw GradleException("app/libs/ircapybara.aar is missing. Run ./build-core.sh to build the Go client core first.")
        }
    }
}

dependencies {
    implementation(files(coreLibrary))
}
