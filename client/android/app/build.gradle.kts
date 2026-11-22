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
            // The gomobile runtime is reached through JNI by name; keep R8 off
            // until keep rules for it are added.
            isMinifyEnabled = false
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    // The web frontend is not copied: the APK packages the same files the
    // desktop shell embeds. They end up under assets/dist/.
    sourceSets["main"].assets.srcDir(rootProject.file("../frontend"))
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
