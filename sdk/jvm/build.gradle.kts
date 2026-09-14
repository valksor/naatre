plugins {
    `java-library`
    kotlin("jvm") version "2.2.20"
    `maven-publish`
}

group = "io.naatre"
version = "0.1.0"

repositories {
    mavenCentral()
}

dependencies {
    api("org.jetbrains.kotlinx:kotlinx-coroutines-core:1.10.2")
}

java {
    sourceCompatibility = JavaVersion.VERSION_17
    targetCompatibility = JavaVersion.VERSION_17
    withSourcesJar()
}

kotlin {
    compilerOptions {
        jvmTarget.set(org.jetbrains.kotlin.gradle.dsl.JvmTarget.JVM_17)
        allWarningsAsErrors.set(true)
        javaParameters.set(true)
    }
}

sourceSets {
    main {
        java.srcDir("generated/java")
        kotlin.srcDir("generated/kotlin")
    }
}

tasks.withType<JavaCompile>().configureEach {
    options.compilerArgs.addAll(listOf("-Xlint:all", "-Werror", "-parameters"))
    options.release.set(17)
}

tasks.register<JavaExec>("jvmConformance") {
    dependsOn(tasks.named("testClasses"))
    classpath = sourceSets["test"].runtimeClasspath
    mainClass.set("io.naatre.sdk.JvmConformanceKt")
    args(rootProject.projectDir.resolve("../..").normalize().absolutePath)
}

tasks.named("check") {
    dependsOn("jvmConformance")
}

publishing {
    publications {
        create<MavenPublication>("jvmSdk") {
            from(components["java"])
            artifactId = "naatre-jvm-sdk"
        }
    }
}
