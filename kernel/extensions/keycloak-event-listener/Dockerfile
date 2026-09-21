# Builds the provider jar and ships it in an image whose only job is to be
# copied from: Keycloak's pod runs it as an init container that places the jar
# in /opt/keycloak/providers. Keycloak's own image stays upstream's, unmodified.
FROM maven:3.9-eclipse-temurin-17 AS build
WORKDIR /src
COPY pom.xml .
RUN mvn -q -B dependency:go-offline
COPY src/ src/
RUN mvn -q -B package

FROM busybox:1.37
COPY --from=build /src/target/gentian-keycloak-event-listener.jar /providers/gentian-keycloak-event-listener.jar
USER 65532:65532
ENTRYPOINT ["cp", "/providers/gentian-keycloak-event-listener.jar", "/target/"]
