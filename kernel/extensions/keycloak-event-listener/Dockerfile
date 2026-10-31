# Builds the provider jar and ships it in an image whose only job is to be
# copied from: Keycloak's pod runs it as an init container that places the jar
# in /opt/keycloak/providers. Keycloak's own image stays upstream's, unmodified.
# On the build platform: a jar is the same bytes for every architecture, and
# under emulation Maven took four times as long to produce it.
FROM --platform=$BUILDPLATFORM maven:3.9-eclipse-temurin-26@sha256:b2c1ad85954592f9928e84327c65201f308ad9b5d8ed7d5b823717c97bf23fbb AS build
WORKDIR /src
COPY pom.xml .
RUN mvn -q -B dependency:go-offline
COPY src/ src/
RUN mvn -q -B package

FROM busybox:1.37@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e
COPY --from=build /src/target/gentian-keycloak-event-listener.jar /providers/gentian-keycloak-event-listener.jar
USER 65532:65532
ENTRYPOINT ["cp", "/providers/gentian-keycloak-event-listener.jar", "/target/"]
