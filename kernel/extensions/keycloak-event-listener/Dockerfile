# Builds the provider jar and ships it in an image whose only job is to be
# copied from: Keycloak's pod runs it as an init container that places the jar
# in /opt/keycloak/providers. Keycloak's own image stays upstream's, unmodified.
# On the build platform: a jar is the same bytes for every architecture, and
# under emulation Maven took four times as long to produce it.
FROM --platform=$BUILDPLATFORM maven:3.9-eclipse-temurin-17@sha256:1a352420f7aba21f5ad08df31bab55f74c013fb491f1ae8ab1dd7ff9ed698584 AS build
WORKDIR /src
COPY pom.xml .
RUN mvn -q -B dependency:go-offline
COPY src/ src/
RUN mvn -q -B package

FROM busybox:1.37@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e
COPY --from=build /src/target/gentian-keycloak-event-listener.jar /providers/gentian-keycloak-event-listener.jar
USER 65532:65532
ENTRYPOINT ["cp", "/providers/gentian-keycloak-event-listener.jar", "/target/"]
