plugins {
    `java-library`
    `maven-publish`
}

group = "io.naatre"
version = "0.1.0"

java {
    sourceCompatibility = JavaVersion.VERSION_17
    targetCompatibility = JavaVersion.VERSION_17
    withSourcesJar()
}

dependencies {
    api(project(":"))
}

tasks.withType<JavaCompile>().configureEach {
    options.compilerArgs.addAll(listOf("-Xlint:all", "-Werror", "-parameters"))
    options.release.set(17)
}

tasks.register<JavaExec>("httpConformance") {
    dependsOn(tasks.named("testClasses"))
    classpath = sourceSets["test"].runtimeClasspath
    mainClass.set("io.naatre.sdk.http.JavaHttpTransportConformance")
}

tasks.named("check") {
    dependsOn("httpConformance")
}

publishing {
    publications {
        create<MavenPublication>("javaHttp") {
            from(components["java"])
            artifactId = "naatre-jvm-http"
        }
    }
}
