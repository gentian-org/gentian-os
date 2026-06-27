<!--
SPDX-FileCopyrightText: 2023 Bundesministerium des Innern und für Heimat, PG ZenDiS "Projektgruppe für Aufbau ZenDiS"
SPDX-License-Identifier: Apache-2.0
-->
# mariadb

A Helm chart for running MariaDB database

## Gentian deployment

Kernel installs this chart via Crossplane `Release`. Packaged charts are published
to `charts/infra/packages/`; run `./scripts/publish-infra-charts.sh` after editing
the chart source under `charts/infra/mariadb/`.

## Standalone install

```console
helm install my-release ./charts/infra/mariadb
```

## Requirements

| Repository | Name | Version |
|------------|------|---------|
| https://charts.bitnami.com/bitnami | common | ^2.x.x |

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{}` | Affinity for pod assignment. Ref: https://kubernetes.io/docs/concepts/configuration/assign-pod-node/#affinity-and-anti-affinity Note: podAffinityPreset, podAntiAffinityPreset, and nodeAffinityPreset will be ignored when it's set. |
| cleanup.deletePodsOnSuccess | bool | `true` | Keep Pods/Job logs after successful run. |
| cleanup.deletePodsOnSuccessTimeout | int | `3600` | When deletePodsOnSuccess is enabled, the pod will be deleted after configured seconds. |
| commonAnnotations | object | `{}` | Additional custom annotations to add to all deployed objects. |
| commonLabels | object | `{}` | Additional custom labels to add to all deployed objects. |
| containerSecurityContext.allowPrivilegeEscalation | bool | `false` | Enable container privileged escalation. |
| containerSecurityContext.capabilities | object | `{"drop":["ALL"]}` | Security capabilities for container. |
| containerSecurityContext.enabled | bool | `true` | Enable security context. |
| containerSecurityContext.privileged | bool | `false` | Run in privileged mode. |
| containerSecurityContext.readOnlyRootFilesystem | bool | `true` | Mounts the container's root filesystem as read-only. |
| containerSecurityContext.runAsGroup | int | `1001` | Process group id. |
| containerSecurityContext.runAsNonRoot | bool | `true` | Run container as user. |
| containerSecurityContext.runAsUser | int | `1001` | Process user id. |
| containerSecurityContext.seccompProfile.type | string | `"RuntimeDefault"` | Disallow custom Seccomp profile by setting it to RuntimeDefault. |
| extraEnvVars | list | `[]` | Array with extra environment variables to add to containers.  extraEnvVars:   - name: FOO     value: "bar"  |
| extraEnvVarsCM | string | `""` | Name of existing ConfigMap containing extra env vars. |
| extraEnvVarsSecret | string | `""` | Name of existing Secret containing extra env vars. |
| extraPodSpec | object | `{}` | Additional pod specs. |
| extraVolumeMounts | list | `[]` | Optionally specify extra list of additional volumeMounts. |
| extraVolumes | list | `[]` | Optionally specify extra list of additional volumes. |
| fullnameOverride | string | `""` | Provide a name to substitute for the full names of resources. |
| global.imagePullSecrets | list | `[]` | Credentials to fetch images from private registry. Ref: https://kubernetes.io/docs/tasks/configure-pod-container/pull-image-private-registry/  imagePullSecrets:   - "docker-registry"  |
| global.registry | string | `"docker.io"` | Container registry address. |
| hostIPC | bool | `false` | Specify if host IPC should be enabled for pod. |
| hostNetwork | bool | `false` | Specify if host network should be enabled for pod. |
| image.imagePullPolicy | string | `"IfNotPresent"` | Define an ImagePullPolicy.  Ref.: https://kubernetes.io/docs/concepts/containers/images/#image-pull-policy  "IfNotPresent" => The image is pulled only if it is not already present locally. "Always" => Every time the kubelet launches a container, the kubelet queries the container image registry to             resolve the name to an image digest. If the kubelet has a container image with that exact digest cached             locally, the kubelet uses its cached image; otherwise, the kubelet pulls the image with the resolved             digest, and uses that image to launch the container. "Never" => The kubelet does not try fetching the image. If the image is somehow already present locally, the            kubelet attempts to start the container; otherwise, startup fails. |
| image.registry | string | `""` | Container registry address. This setting has higher precedence than global.registry. |
| image.repository | string | `"mariadb"` | Container repository string. |
| image.tag | string | `"11.1.2-jammy@sha256:b6440c4f4e1471bdcee202e4c4e21c1f93af87421f6d33028363dd224e54f481"` | Define image tag. |
| imagePullSecrets | list | `[]` | Credentials to fetch images from private registry. Ref: https://kubernetes.io/docs/tasks/configure-pod-container/pull-image-private-registry/  imagePullSecrets:   - "docker-registry"  |
| job.databases | list | `[]` | Bootstrap databases into mariadb server. When databases already exists, they will stay untouched.  databases:  - name: "name_of_database"    user: "existing_user_which_will_get_grant" |
| job.enabled | bool | `true` | Enable database bootstrapping. |
| job.image | object | `{"imagePullPolicy":"IfNotPresent","registry":"","repository":"mariadb","tag":"11.1.2-jammy@sha256:b6440c4f4e1471bdcee202e4c4e21c1f93af87421f6d33028363dd224e54f481"}` | Job image. |
| job.image.imagePullPolicy | string | `"IfNotPresent"` | Define an ImagePullPolicy.  Ref.: https://kubernetes.io/docs/concepts/containers/images/#image-pull-policy  "IfNotPresent" => The image is pulled only if it is not already present locally. "Always" => Every time the kubelet launches a container, the kubelet queries the container image registry to             resolve the name to an image digest. If the kubelet has a container image with that exact digest             cached locally, the kubelet uses its cached image; otherwise, the kubelet pulls the image with the             resolved digest, and uses that image to launch the container. "Never" => The kubelet does not try fetching the image. If the image is somehow already present locally, the            kubelet attempts to start the container; otherwise, startup fails.  |
| job.image.registry | string | `""` | Container registry address. This setting has higher precedence than global.registry. |
| job.image.repository | string | `"mariadb"` | Container repository string. |
| job.image.tag | string | `"11.1.2-jammy@sha256:b6440c4f4e1471bdcee202e4c4e21c1f93af87421f6d33028363dd224e54f481"` | Define image tag. |
| job.retries | int | `6` | Amount of retries while waiting for mariadb server is available. |
| job.users | list | `[]` | Bootstrap users into mariadb server. When a user already exists its password and user specific connectionLimit are updated.  users:   - username: "name_of_user"     password: "RandomPassword0#"     connectionLimit: 10 |
| job.wait | int | `10` | Time to wait in each wait in each iteration until mariadb server is available. |
| lifecycleHooks | object | `{}` | Lifecycle to automate configuration before or after startup. |
| livenessProbe.enabled | bool | `true` | Enables kubernetes LivenessProbe. |
| livenessProbe.failureThreshold | int | `6` | Number of failed executions until container is terminated. |
| livenessProbe.initialDelaySeconds | int | `15` | Delay after container start until LivenessProbe is executed. |
| livenessProbe.periodSeconds | int | `10` | Time between probe executions. |
| livenessProbe.successThreshold | int | `1` | Number of successful executions after failed ones until container is marked healthy. |
| livenessProbe.timeoutSeconds | int | `5` | Timeout for command return. |
| mariadb.configuration | string | `""` | MariaDB additional configuration to be injected as ConfigMap. |
| mariadb.rootPassword.existingSecret.key | string | `""` | Key inside the Kubernetes secret that contains the root password for the database. |
| mariadb.rootPassword.existingSecret.name | string | `""` | Name of the Kubernetes secret that contains the root password for the database. |
| mariadb.rootPassword.value | string | `"BETTER_CHANGE_ME"` | Database root password as plain value. |
| nameOverride | string | `""` | String to partially override release name. |
| nodeSelector | object | `{}` | Node labels for pod assignment. Ref: https://kubernetes.io/docs/user-guide/node-selection/ |
| persistence.accessModes | list | `["ReadWriteOnce"]` | The volume access modes, some of "ReadWriteOnce", "ReadOnlyMany", "ReadWriteMany", "ReadWriteOncePod".  "ReadWriteOnce" => The volume can be mounted as read-write by a single node. ReadWriteOnce access mode still can                    allow multiple pods to access the volume when the pods are running on the same node. "ReadOnlyMany" => The volume can be mounted as read-only by many nodes. "ReadWriteMany" => The volume can be mounted as read-write by many nodes. "ReadWriteOncePod" => The volume can be mounted as read-write by a single Pod. Use ReadWriteOncePod access mode if                       you want to ensure that only one pod across whole cluster can read that PVC or write to it.  |
| persistence.annotations | object | `{}` | Annotations for the PVC. |
| persistence.dataSource | object | `{}` | Custom PVC data source. |
| persistence.enabled | bool | `true` | Enable data persistence (true) or use temporary storage (false). |
| persistence.existingClaim | string | `""` | Use an already existing claim. |
| persistence.labels | object | `{}` | Labels for the PVC. |
| persistence.selector | object | `{}` | Selector to match an existing Persistent Volume (this value is evaluated as a template)  selector:   matchLabels:     app: my-app  |
| persistence.size | string | `"8Gi"` | The volume size with unit. |
| persistence.storageClass | string | `""` | The (storage) class of PV. |
| podAnnotations | object | `{}` | Pod Annotations. Ref: https://kubernetes.io/docs/concepts/overview/working-with-objects/annotations/ |
| podLabels | object | `{}` | Pod Labels. Ref: https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/ |
| podSecurityContext.enabled | bool | `true` | Enable security context. |
| podSecurityContext.fsGroup | int | `1001` | If specified, all processes of the container are also part of the supplementary group. |
| podSecurityContext.fsGroupChangePolicy | string | `"OnRootMismatch"` | Change ownership and permission of the volume before being exposed inside a Pod. |
| priorityClassName | string | `""` | Define PriorityClass name for pod. Ref: https://kubernetes.io/docs/concepts/scheduling-eviction/pod-priority-preemption/#priorityclass |
| readinessProbe.enabled | bool | `true` | Enables kubernetes ReadinessProbe. |
| readinessProbe.failureThreshold | int | `6` | Number of failed executions until container is terminated. |
| readinessProbe.initialDelaySeconds | int | `5` | Delay after container start until ReadinessProbe is executed. |
| readinessProbe.periodSeconds | int | `10` | Time between probe executions. |
| readinessProbe.successThreshold | int | `1` | Number of successful executions after failed ones until container is marked healthy. |
| readinessProbe.timeoutSeconds | int | `5` | Timeout for command return. |
| replicaCount | int | `1` | Set the amount of replicas of deployment. |
| resources.limits.cpu | int | `1` | The max amount of CPUs to consume. |
| resources.limits.memory | string | `"1Gi"` | The max amount of RAM to consume. |
| resources.requests.cpu | string | `"250m"` | The amount of CPUs which has to be available on the scheduled node. |
| resources.requests.memory | string | `"256Mi"` | The amount of RAM which has to be available on the scheduled node. |
| schedulerName | string | `""` | Configure name of Scheduler. |
| service.annotations | object | `{}` | Additional custom annotations. |
| service.clusterIP | string | `""` | Define a cluster if needed. |
| service.enabled | bool | `true` | Enable kubernetes service creation. |
| service.externalTrafficPolicy | string | `""` | The externalTrafficPolicy. |
| service.loadBalancerSourceRanges | string | `""` | The loadBalancerSourceRanges. |
| service.ports.mariadb.containerPort | int | `3306` | Internal port. |
| service.ports.mariadb.port | int | `3306` | Accessible port. |
| service.ports.mariadb.protocol | string | `"TCP"` | Service protocol. |
| service.sessionAffinity | string | `""` | Session affinity type of service. |
| service.sessionAffinityConfig | object | `{}` | Session affinity configuration. |
| service.type | string | `"ClusterIP"` | Choose the kind of Service, one of "ClusterIP", "NodePort" or "LoadBalancer". |
| serviceAccount.annotations | object | `{}` | Additional custom annotations for the ServiceAccount. |
| serviceAccount.automountServiceAccountToken | bool | `false` | Allows auto mount of ServiceAccountToken on the serviceAccount created. Can be set to false if pods using this serviceAccount do not need to use K8s API. |
| serviceAccount.create | bool | `true` | Enable creation of ServiceAccount for pod. |
| serviceAccount.labels | object | `{}` | Additional custom labels for the ServiceAccount. |
| startupProbe.enabled | bool | `true` | Enables kubernetes ReadinessProbe. |
| startupProbe.failureThreshold | int | `15` | Number of failed executions until container is terminated. |
| startupProbe.initialDelaySeconds | int | `15` | Delay after container start until ReadinessProbe is executed. |
| startupProbe.periodSeconds | int | `10` | Time between probe executions. |
| startupProbe.successThreshold | int | `1` | Number of successful executions after failed ones until container is marked healthy. |
| startupProbe.timeoutSeconds | int | `1` | Timeout for command return. |
| terminationGracePeriodSeconds | int | `120` | In seconds, time the given to the pod needs to terminate gracefully. Ref: https://kubernetes.io/docs/concepts/workloads/pods/pod/#termination-of-pods |
| tolerations | list | `[]` | Tolerations for pod assignment. Ref: https://kubernetes.io/docs/concepts/configuration/taint-and-toleration/ |
| topologySpreadConstraints | list | `[]` | Topology spread constraints rely on node labels to identify the topology domain(s) that each Node is in. Ref: https://kubernetes.io/docs/concepts/workloads/pods/pod-topology-spread-constraints/  topologySpreadConstraints:   - maxSkew: 1     topologyKey: failure-domain.beta.kubernetes.io/zone     whenUnsatisfiable: DoNotSchedule |
| updateStrategy.type | string | `"RollingUpdate"` | Set to Recreate if you use persistent volume that cannot be mounted by more than one pods to make sure the pods is destroyed first. |

## Uninstalling the Chart

```bash
helm uninstall my-release
```

## License

This project uses the following license: Apache-2.0

## Copyright

Copyright (C) 2023 Bundesministerium des Innern und für Heimat, PG ZenDiS "Projektgruppe für Aufbau ZenDiS"
