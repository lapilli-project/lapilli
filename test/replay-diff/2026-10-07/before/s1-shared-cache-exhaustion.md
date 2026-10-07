commands: 403 | the same: 217 | the same lines in another order: 1 | both fail, worded differently: 3 | the cluster moved between the two live passes: 0 | **differ: 182**

| question | asked | same | order | worded | moved | differ |
|---|---|---|---|---|---|---|
| `get events one named --field-selector` | 7 | 0 | 0 | 0 | 0 | 7 |
| `get pod one named -o wide` | 7 | 0 | 0 | 0 | 0 | 7 |
| `get events one named -o wide` | 4 | 0 | 0 | 0 | 0 | 4 |
| `get pods one named -o wide` | 5 | 0 | 0 | 1 | 0 | 4 |
| `get apiservices.apiregistration.k8s.io one named -o wide` | 3 | 0 | 0 | 0 | 0 | 3 |
| `get certificatesigningrequests.certificates.k8s.io one named -o wide` | 3 | 0 | 0 | 0 | 0 | 3 |
| `get clusterrolebindings.rbac.authorization.k8s.io one named -o wide` | 3 | 0 | 0 | 0 | 0 | 3 |
| `get clusterroles.rbac.authorization.k8s.io one named -o wide` | 3 | 0 | 0 | 0 | 0 | 3 |
| `get controllerrevisions.apps one named -o wide` | 3 | 0 | 0 | 0 | 0 | 3 |
| `get csinodes.storage.k8s.io one named -o wide` | 3 | 0 | 0 | 0 | 0 | 3 |
| `get daemonsets.apps one named -o wide` | 3 | 0 | 0 | 0 | 0 | 3 |
| `get deployments.apps one named -o wide` | 3 | 0 | 0 | 0 | 0 | 3 |
| `get flowschemas.flowcontrol.apiserver.k8s.io one named -o wide` | 3 | 0 | 0 | 0 | 0 | 3 |
| `get ipaddresses.networking.k8s.io one named -o wide` | 3 | 0 | 0 | 0 | 0 | 3 |
| `get nodes one named -o wide` | 3 | 0 | 0 | 0 | 0 | 3 |
| `get priorityclasses.scheduling.k8s.io one named -o wide` | 3 | 0 | 0 | 0 | 0 | 3 |
| `get prioritylevelconfigurations.flowcontrol.apiserver.k8s.io one named -o wide` | 3 | 0 | 0 | 0 | 0 | 3 |
| `get replicasets.apps one named -o wide` | 3 | 0 | 0 | 0 | 0 | 3 |
| `get rolebindings.rbac.authorization.k8s.io one named -o wide` | 3 | 0 | 0 | 0 | 0 | 3 |
| `get roles.rbac.authorization.k8s.io one named -o wide` | 3 | 0 | 0 | 0 | 0 | 3 |
| `rollout --timeout` | 4 | 1 | 0 | 0 | 0 | 3 |
| `describe rolebindings.rbac.authorization.k8s.io` | 2 | 0 | 0 | 0 | 0 | 2 |
| `describe roles.rbac.authorization.k8s.io` | 2 | 0 | 0 | 0 | 0 | 2 |
| `explain` | 2 | 0 | 0 | 0 | 0 | 2 |
| `get apiservices.apiregistration.k8s.io one named` | 2 | 0 | 0 | 0 | 0 | 2 |
| `get certificatesigningrequests.certificates.k8s.io one named` | 2 | 0 | 0 | 0 | 0 | 2 |
| `get clusterrolebindings.rbac.authorization.k8s.io one named` | 2 | 0 | 0 | 0 | 0 | 2 |
| `get clusterroles.rbac.authorization.k8s.io one named` | 2 | 0 | 0 | 0 | 0 | 2 |
| `get clustertrustbundles.certificates.k8s.io one named -o wide` | 2 | 0 | 0 | 0 | 0 | 2 |
| `get configmaps one named` | 2 | 0 | 0 | 0 | 0 | 2 |
| `get configmaps one named -o wide` | 3 | 1 | 0 | 0 | 0 | 2 |
| `get controllerrevisions.apps one named` | 2 | 0 | 0 | 0 | 0 | 2 |
| `get csinodes.storage.k8s.io one named` | 2 | 0 | 0 | 0 | 0 | 2 |
| `get daemonsets.apps one named` | 2 | 0 | 0 | 0 | 0 | 2 |
| `get deployments.apps one named` | 2 | 0 | 0 | 0 | 0 | 2 |
| `get endpoints one named -o wide` | 2 | 0 | 0 | 0 | 0 | 2 |
| `get endpointslices.discovery.k8s.io one named -o wide` | 2 | 0 | 0 | 0 | 0 | 2 |
| `get events one named` | 2 | 0 | 0 | 0 | 0 | 2 |
| `get events one named --sort-by` | 2 | 0 | 0 | 0 | 0 | 2 |
| `get flowschemas.flowcontrol.apiserver.k8s.io one named` | 2 | 0 | 0 | 0 | 0 | 2 |
| `get ipaddresses.networking.k8s.io one named` | 2 | 0 | 0 | 0 | 0 | 2 |
| `get namespaces one named` | 2 | 0 | 0 | 0 | 0 | 2 |
| `get namespaces one named -o wide` | 3 | 1 | 0 | 0 | 0 | 2 |
| `get nodes one named` | 2 | 0 | 0 | 0 | 0 | 2 |
| `get pods one named` | 3 | 1 | 0 | 0 | 0 | 2 |
| `get pods one named --field-selector -o wide` | 2 | 0 | 0 | 0 | 0 | 2 |
| `get pods one named --sort-by` | 2 | 0 | 0 | 0 | 0 | 2 |
| `get priorityclasses.scheduling.k8s.io one named` | 2 | 0 | 0 | 0 | 0 | 2 |
| `get prioritylevelconfigurations.flowcontrol.apiserver.k8s.io one named` | 2 | 0 | 0 | 0 | 0 | 2 |
| `get replicasets.apps one named` | 2 | 0 | 0 | 0 | 0 | 2 |
| `get rolebindings.rbac.authorization.k8s.io one named` | 2 | 0 | 0 | 0 | 0 | 2 |
| `get roles.rbac.authorization.k8s.io one named` | 2 | 0 | 0 | 0 | 0 | 2 |
| `get servicecidrs.networking.k8s.io one named -o wide` | 2 | 0 | 0 | 0 | 0 | 2 |
| `get services one named -o wide` | 2 | 0 | 0 | 0 | 0 | 2 |
| `get storageclasses.storage.k8s.io one named -o wide` | 2 | 0 | 0 | 0 | 0 | 2 |
| `cluster-info` | 1 | 0 | 0 | 0 | 0 | 1 |
| `describe clustertrustbundles.certificates.k8s.io` | 1 | 0 | 0 | 0 | 0 | 1 |
| `describe endpoints` | 1 | 0 | 0 | 0 | 0 | 1 |
| `describe secrets` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get all one named` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get all one named -o wide` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get apiservices.apiregistration.k8s.io` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get certificatesigningrequests.certificates.k8s.io` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get clusterroles.rbac.authorization.k8s.io` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get clustertrustbundles.certificates.k8s.io` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get clustertrustbundles.certificates.k8s.io one named` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get configmaps,secrets,persistentvolumeclaims one named` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get controllerrevisions.apps` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get csinodes.storage.k8s.io` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get endpoints` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get endpoints one named` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get endpoints one named -o yaml` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get endpointslices one named -o wide` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get endpointslices.discovery.k8s.io` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get endpointslices.discovery.k8s.io one named` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get events` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get events one named --sort-by -o wide` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get flowschemas.flowcontrol.apiserver.k8s.io` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get ipaddresses.networking.k8s.io` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get networkpolicies.networking.k8s.io one named` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get networkpolicies.networking.k8s.io one named -o wide` | 2 | 1 | 0 | 0 | 0 | 1 |
| `get nodes` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get pods one named --sort-by -o wide` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get pods one named -l -o wide` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get pods,services one named -o wide` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get priorityclasses.scheduling.k8s.io` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get prioritylevelconfigurations.flowcontrol.apiserver.k8s.io` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get replicasets.apps` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get roles.rbac.authorization.k8s.io` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get secrets one named` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get secrets one named -o wide` | 2 | 1 | 0 | 0 | 0 | 1 |
| `get servicecidrs.networking.k8s.io` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get servicecidrs.networking.k8s.io one named` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get services one named` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get storageclasses.storage.k8s.io` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get storageclasses.storage.k8s.io one named` | 1 | 0 | 0 | 0 | 0 | 1 |
| `get clusterrolebindings.rbac.authorization.k8s.io` | 1 | 0 | 1 | 0 | 0 |  |
| `get events one named --field-selector --sort-by` | 1 | 0 | 0 | 1 | 0 |  |
| `logs --tail` | 25 | 24 | 0 | 1 | 0 |  |

The same every time: 131 kinds of question, 187 commands.

### Differ

- `kubectl get apiservices.apiregistration.k8s.io -A`
  - live: NAME SERVICE AVAILABLE AGE | frozen: NAME AGE
- `kubectl get apiservices.apiregistration.k8s.io -A -o wide`
  - live: NAME SERVICE AVAILABLE AGE | frozen: NAME AGE
- `kubectl get apiservices.apiregistration.k8s.io v1.`
  - live: NAME SERVICE AVAILABLE AGE | frozen: NAME AGE
- `kubectl get apiservices.apiregistration.k8s.io v1. -o wide`
  - live: NAME SERVICE AVAILABLE AGE | frozen: NAME AGE
- `kubectl get apiservices.apiregistration.k8s.io v1.admissionregistration.k8s.io`
  - live: NAME SERVICE AVAILABLE AGE | frozen: NAME AGE
- `kubectl get certificatesigningrequests.certificates.k8s.io -A`
  - live: NAME AGE SIGNERNAME REQUESTOR REQUESTEDDURATION CONDITION | frozen: NAME AGE
- `kubectl get certificatesigningrequests.certificates.k8s.io -A -o wide`
  - live: NAME AGE SIGNERNAME REQUESTOR REQUESTEDDURATION CONDITION | frozen: NAME AGE
- `kubectl get certificatesigningrequests.certificates.k8s.io csr-9bf5g`
  - live: NAME AGE SIGNERNAME REQUESTOR REQUESTEDDURATION CONDITION | frozen: NAME AGE
- `kubectl get certificatesigningrequests.certificates.k8s.io csr-9bf5g -o wide`
  - live: NAME AGE SIGNERNAME REQUESTOR REQUESTEDDURATION CONDITION | frozen: NAME AGE
- `kubectl get certificatesigningrequests.certificates.k8s.io csr-ls794`
  - live: NAME AGE SIGNERNAME REQUESTOR REQUESTEDDURATION CONDITION | frozen: NAME AGE
- `kubectl get clusterrolebindings.rbac.authorization.k8s.io -A -o wide`
  - live: NAME ROLE AGE USERS GROUPS SERVICEACCOUNTS | frozen: NAME ROLE AGE
- `kubectl get clusterrolebindings.rbac.authorization.k8s.io cluster-admin`
  - live: NAME ROLE AGE | frozen: NAME AGE
- `kubectl get clusterrolebindings.rbac.authorization.k8s.io cluster-admin -o wide`
  - live: NAME ROLE AGE USERS GROUPS SERVICEACCOUNTS | frozen: NAME AGE
- `kubectl get clusterrolebindings.rbac.authorization.k8s.io kindnet`
  - live: NAME ROLE AGE | frozen: NAME AGE
- `kubectl get clusterroles.rbac.authorization.k8s.io -A`
  - live: NAME CREATED AT | frozen: NAME AGE
- `kubectl get clusterroles.rbac.authorization.k8s.io -A -o wide`
  - live: NAME CREATED AT | frozen: NAME AGE
- `kubectl get clusterroles.rbac.authorization.k8s.io admin`
  - live: NAME CREATED AT | frozen: NAME AGE
- `kubectl get clusterroles.rbac.authorization.k8s.io admin -o wide`
  - live: NAME CREATED AT | frozen: NAME AGE
- `kubectl get clusterroles.rbac.authorization.k8s.io cluster-admin`
  - live: NAME CREATED AT | frozen: NAME AGE
- `kubectl get clustertrustbundles.certificates.k8s.io -A`
  - live: NAME SIGNERNAME | frozen: NAME AGE
- `kubectl get clustertrustbundles.certificates.k8s.io -A -o wide`
  - live: NAME SIGNERNAME | frozen: NAME AGE
- `kubectl get clustertrustbundles.certificates.k8s.io kubernetes.io:kube-apiserver-serving:79c3c92ecfe5c423c6e1f9b6`
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource (get clustertrustbundles.certificates.k8s.io kubernetes.io:kube-apiserver-serving:79c3c92ecfe5c423c6e1f9b6)
- `kubectl get clustertrustbundles.certificates.k8s.io kubernetes.io:kube-apiserver-serving:79c3c92ecfe5c423c6e1f9b6 -o wide`
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource (get clustertrustbundles.certificates.k8s.io kubernetes.io:kube-apiserver-serving:79c3c92ecfe5c423c6e1f9b6)
- `kubectl describe clustertrustbundles.certificates.k8s.io kubernetes.io:kube-apiserver-serving:79c3c92ecfe5c423c6e1f9b6`
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource
- `kubectl get configmaps kube-root-ca.crt -n shop`
  - live: NAME DATA AGE | frozen: NAME AGE
- `kubectl get configmaps kube-root-ca.crt -n shop -o wide`
  - live: NAME DATA AGE | frozen: NAME AGE
- `kubectl get configmaps report-worker-config -n shop`
  - live: NAME DATA AGE | frozen: NAME AGE
- `kubectl get configmaps report-worker-config -n shop -o wide`
  - live: NAME DATA AGE | frozen: NAME AGE
- `kubectl get controllerrevisions.apps -A`
  - live: NAMESPACE NAME CONTROLLER REVISION AGE | frozen: NAMESPACE NAME AGE
- `kubectl get controllerrevisions.apps -A -o wide`
  - live: NAMESPACE NAME CONTROLLER REVISION AGE | frozen: NAMESPACE NAME AGE
- `kubectl get controllerrevisions.apps kindnet-5544ddccdf -n kube-system`
  - live: NAME CONTROLLER REVISION AGE | frozen: NAME AGE
- `kubectl get controllerrevisions.apps kindnet-5544ddccdf -n kube-system -o wide`
  - live: NAME CONTROLLER REVISION AGE | frozen: NAME AGE
- `kubectl get controllerrevisions.apps kube-proxy-6f6bf99bf5 -n kube-system`
  - live: NAME CONTROLLER REVISION AGE | frozen: NAME AGE
- `kubectl get csinodes.storage.k8s.io -A`
  - live: NAME DRIVERS AGE | frozen: NAME AGE
- `kubectl get csinodes.storage.k8s.io -A -o wide`
  - live: NAME DRIVERS AGE | frozen: NAME AGE
- `kubectl get csinodes.storage.k8s.io rcabench-control-plane`
  - live: NAME DRIVERS AGE | frozen: NAME AGE
- `kubectl get csinodes.storage.k8s.io rcabench-control-plane -o wide`
  - live: NAME DRIVERS AGE | frozen: NAME AGE
- `kubectl get csinodes.storage.k8s.io rcabench-worker`
  - live: NAME DRIVERS AGE | frozen: NAME AGE
- `kubectl get daemonsets.apps -A -o wide`
  - live: NAMESPACE NAME DESIRED CURRENT READY UP-TO-DATE AVAILABLE NODE SELECTOR AGE CONTAINERS IMAGES SELECTOR | frozen: NAMESPACE NAME DESIRED CURRENT READY UP-TO-DATE AVAILABLE NODE SELECTOR AGE
- `kubectl get daemonsets.apps kindnet -n kube-system`
  - live: NAME DESIRED CURRENT READY UP-TO-DATE AVAILABLE NODE SELECTOR AGE | frozen: NAME AGE
- `kubectl get daemonsets.apps kindnet -n kube-system -o wide`
  - live: NAME DESIRED CURRENT READY UP-TO-DATE AVAILABLE NODE SELECTOR AGE CONTAINERS IMAGES SELECTOR | frozen: NAME AGE
- `kubectl get daemonsets.apps kube-proxy -n kube-system`
  - live: NAME DESIRED CURRENT READY UP-TO-DATE AVAILABLE NODE SELECTOR AGE | frozen: NAME AGE
- `kubectl get deployments.apps -A -o wide`
  - live: NAMESPACE NAME READY UP-TO-DATE AVAILABLE AGE CONTAINERS IMAGES SELECTOR | frozen: NAMESPACE NAME READY UP-TO-DATE AVAILABLE AGE
- `kubectl get deployments.apps cache -n shop`
  - live: NAME READY UP-TO-DATE AVAILABLE AGE | frozen: NAME AGE
- `kubectl get deployments.apps cache -n shop -o wide`
  - live: NAME READY UP-TO-DATE AVAILABLE AGE CONTAINERS IMAGES SELECTOR | frozen: NAME AGE
- `kubectl get deployments.apps checkout-api -n shop`
  - live: NAME READY UP-TO-DATE AVAILABLE AGE | frozen: NAME AGE
- `kubectl get endpoints -A`
  - live: NAMESPACE NAME ENDPOINTS AGE | frozen: NAMESPACE NAME AGE
- `kubectl get endpoints -A -o wide`
  - live: NAMESPACE NAME ENDPOINTS AGE | frozen: NAMESPACE NAME AGE
- `kubectl get endpoints cache -n shop`
  - live: NAME ENDPOINTS AGE | frozen: NAME AGE
- `kubectl get endpoints cache -n shop -o wide`
  - live: NAME ENDPOINTS AGE | frozen: NAME AGE
- `kubectl describe endpoints cache -n shop`
  - stderr, live: Warning: v1 Endpoints is deprecated in v1.33+; use discovery.k8s.io/v1 EndpointSlice | frozen: (nothing)
- `kubectl get endpointslices.discovery.k8s.io -A`
  - live: NAMESPACE NAME ADDRESSTYPE PORTS ENDPOINTS AGE | frozen: NAMESPACE NAME AGE
- `kubectl get endpointslices.discovery.k8s.io -A -o wide`
  - live: NAMESPACE NAME ADDRESSTYPE PORTS ENDPOINTS AGE | frozen: NAMESPACE NAME AGE
- `kubectl get endpointslices.discovery.k8s.io cache-z924b -n shop`
  - live: NAME ADDRESSTYPE PORTS ENDPOINTS AGE | frozen: NAME AGE
- `kubectl get endpointslices.discovery.k8s.io cache-z924b -n shop -o wide`
  - live: NAME ADDRESSTYPE PORTS ENDPOINTS AGE | frozen: NAME AGE
- `kubectl get events -A`
  - live: NAMESPACE LAST SEEN TYPE REASON OBJECT MESSAGE | frozen: NAMESPACE LASTTIMESTAMP TYPE REASON OBJECT MESSAGE
- `kubectl get events -A -o wide`
  - live: NAMESPACE LAST SEEN TYPE REASON OBJECT SUBOBJECT SOURCE MESSAGE FIRST SEEN COUNT NAME | frozen: NAMESPACE LASTTIMESTAMP TYPE REASON OBJECT MESSAGE
- `kubectl get events cache-67765b96c6-smqjt.18dc3d131d21a7df -n shop`
  - live: LAST SEEN TYPE REASON OBJECT MESSAGE | frozen: NAME AGE
- `kubectl get events cache-67765b96c6-smqjt.18dc3d131d21a7df -n shop -o wide`
  - live: LAST SEEN TYPE REASON OBJECT SUBOBJECT SOURCE MESSAGE FIRST SEEN COUNT NAME | frozen: NAME AGE
- `kubectl get events cache-67765b96c6-smqjt.18dc3d159f1fdc90 -n shop`
  - live: LAST SEEN TYPE REASON OBJECT MESSAGE | frozen: NAME AGE
- `kubectl get flowschemas.flowcontrol.apiserver.k8s.io -A`
  - live: NAME PRIORITYLEVEL MATCHINGPRECEDENCE DISTINGUISHERMETHOD AGE MISSINGPL | frozen: NAME AGE
- `kubectl get flowschemas.flowcontrol.apiserver.k8s.io -A -o wide`
  - live: NAME PRIORITYLEVEL MATCHINGPRECEDENCE DISTINGUISHERMETHOD AGE MISSINGPL | frozen: NAME AGE
- `kubectl get flowschemas.flowcontrol.apiserver.k8s.io catch-all`
  - live: NAME PRIORITYLEVEL MATCHINGPRECEDENCE DISTINGUISHERMETHOD AGE MISSINGPL | frozen: NAME AGE
- `kubectl get flowschemas.flowcontrol.apiserver.k8s.io catch-all -o wide`
  - live: NAME PRIORITYLEVEL MATCHINGPRECEDENCE DISTINGUISHERMETHOD AGE MISSINGPL | frozen: NAME AGE
- `kubectl get flowschemas.flowcontrol.apiserver.k8s.io exempt`
  - live: NAME PRIORITYLEVEL MATCHINGPRECEDENCE DISTINGUISHERMETHOD AGE MISSINGPL | frozen: NAME AGE
- `kubectl get ipaddresses.networking.k8s.io -A`
  - live: NAME PARENTREF | frozen: NAME AGE
- `kubectl get ipaddresses.networking.k8s.io -A -o wide`
  - live: NAME PARENTREF | frozen: NAME AGE
- `kubectl get ipaddresses.networking.k8s.io 10.96.0.1`
  - live: NAME PARENTREF | frozen: NAME AGE
- `kubectl get ipaddresses.networking.k8s.io 10.96.0.1 -o wide`
  - live: NAME PARENTREF | frozen: NAME AGE
- `kubectl get ipaddresses.networking.k8s.io 10.96.0.10`
  - live: NAME PARENTREF | frozen: NAME AGE
- `kubectl get namespaces default`
  - live: NAME STATUS AGE | frozen: NAME AGE
- `kubectl get namespaces default -o wide`
  - live: NAME STATUS AGE | frozen: NAME AGE
- `kubectl get namespaces kube-node-lease`
  - live: NAME STATUS AGE | frozen: NAME AGE
- `kubectl get namespaces kube-node-lease -o wide`
  - live: NAME STATUS AGE | frozen: NAME AGE
- `kubectl get networkpolicies.networking.k8s.io cache-ingress -n shop`
  - live: NAME POD-SELECTOR AGE | frozen: NAME AGE
- `kubectl get networkpolicies.networking.k8s.io cache-ingress -n shop -o wide`
  - live: NAME POD-SELECTOR AGE | frozen: NAME AGE
- `kubectl get nodes -A`
  - live: rcabench-worker Ready <none> <age> v1.37.0 | frozen: rcabench-worker Ready <age> v1.37.0
- `kubectl get nodes -A -o wide`
  - live: rcabench-worker Ready <none> <age> v1.37.0 172.20.0.4 <none> Debian GNU/Linux 13 (trixie) 6.10.14-linuxkit (arm64) containerd://2.3.4 | frozen: rcabench-worker Ready <age> v1.37.0 172.20.0.4 <none> Debian GNU/Linux 13 (trixie) 6.10.14-linuxkit (arm64) containerd://2.3.4
- `kubectl get nodes rcabench-control-plane`
  - live: NAME STATUS ROLES AGE VERSION | frozen: NAME AGE
- `kubectl get nodes rcabench-control-plane -o wide`
  - live: NAME STATUS ROLES AGE VERSION INTERNAL-IP EXTERNAL-IP OS-IMAGE KERNEL-VERSION CONTAINER-RUNTIME | frozen: NAME AGE
- `kubectl get nodes rcabench-worker`
  - live: NAME STATUS ROLES AGE VERSION | frozen: NAME AGE
- `kubectl get pods -A -o wide`
  - live: NAMESPACE NAME READY STATUS RESTARTS AGE IP NODE NOMINATED NODE READINESS GATES | frozen: NAMESPACE NAME READY STATUS RESTARTS AGE
- `kubectl get pods cache-9ffcf47fc-btzxm -n shop`
  - live: NAME READY STATUS RESTARTS AGE | frozen: NAME AGE
- `kubectl get pods cache-9ffcf47fc-btzxm -n shop -o wide`
  - live: NAME READY STATUS RESTARTS AGE IP NODE NOMINATED NODE READINESS GATES | frozen: NAME AGE
- `kubectl get pods checkout-api-58f9d6b876-8m9f4 -n shop`
  - live: NAME READY STATUS RESTARTS AGE | frozen: NAME AGE
- `kubectl get priorityclasses.scheduling.k8s.io -A`
  - live: NAME VALUE GLOBAL-DEFAULT AGE PREEMPTIONPOLICY | frozen: NAME AGE
- `kubectl get priorityclasses.scheduling.k8s.io -A -o wide`
  - live: NAME VALUE GLOBAL-DEFAULT AGE PREEMPTIONPOLICY | frozen: NAME AGE
- `kubectl get priorityclasses.scheduling.k8s.io system-cluster-critical`
  - live: NAME VALUE GLOBAL-DEFAULT AGE PREEMPTIONPOLICY | frozen: NAME AGE
- `kubectl get priorityclasses.scheduling.k8s.io system-cluster-critical -o wide`
  - live: NAME VALUE GLOBAL-DEFAULT AGE PREEMPTIONPOLICY | frozen: NAME AGE
- `kubectl get priorityclasses.scheduling.k8s.io system-node-critical`
  - live: NAME VALUE GLOBAL-DEFAULT AGE PREEMPTIONPOLICY | frozen: NAME AGE
- `kubectl get prioritylevelconfigurations.flowcontrol.apiserver.k8s.io -A`
  - live: NAME TYPE NOMINALCONCURRENCYSHARES QUEUES HANDSIZE QUEUELENGTHLIMIT AGE | frozen: NAME AGE
- `kubectl get prioritylevelconfigurations.flowcontrol.apiserver.k8s.io -A -o wide`
  - live: NAME TYPE NOMINALCONCURRENCYSHARES QUEUES HANDSIZE QUEUELENGTHLIMIT AGE | frozen: NAME AGE
- `kubectl get prioritylevelconfigurations.flowcontrol.apiserver.k8s.io catch-all`
  - live: NAME TYPE NOMINALCONCURRENCYSHARES QUEUES HANDSIZE QUEUELENGTHLIMIT AGE | frozen: NAME AGE
- `kubectl get prioritylevelconfigurations.flowcontrol.apiserver.k8s.io catch-all -o wide`
  - live: NAME TYPE NOMINALCONCURRENCYSHARES QUEUES HANDSIZE QUEUELENGTHLIMIT AGE | frozen: NAME AGE
- `kubectl get prioritylevelconfigurations.flowcontrol.apiserver.k8s.io exempt`
  - live: NAME TYPE NOMINALCONCURRENCYSHARES QUEUES HANDSIZE QUEUELENGTHLIMIT AGE | frozen: NAME AGE
- `kubectl get replicasets.apps -A`
  - live: NAMESPACE NAME DESIRED CURRENT READY AGE | frozen: NAMESPACE NAME AGE
- `kubectl get replicasets.apps -A -o wide`
  - live: NAMESPACE NAME DESIRED CURRENT READY AGE CONTAINERS IMAGES SELECTOR | frozen: NAMESPACE NAME AGE
- `kubectl get replicasets.apps cache-67765b96c6 -n shop`
  - live: NAME DESIRED CURRENT READY AGE | frozen: NAME AGE
- `kubectl get replicasets.apps cache-67765b96c6 -n shop -o wide`
  - live: NAME DESIRED CURRENT READY AGE CONTAINERS IMAGES SELECTOR | frozen: NAME AGE
- `kubectl get replicasets.apps cache-9ffcf47fc -n shop`
  - live: NAME DESIRED CURRENT READY AGE | frozen: NAME AGE
- `kubectl get rolebindings.rbac.authorization.k8s.io -A -o wide`
  - live: NAMESPACE NAME ROLE AGE USERS GROUPS SERVICEACCOUNTS | frozen: NAMESPACE NAME ROLE AGE
- `kubectl get rolebindings.rbac.authorization.k8s.io kubeadm:bootstrap-signer-clusterinfo -n kube-public`
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource (get rolebindings.rbac.authorization.k8s.io kubeadm:bootstrap-signer-clusterinfo)
- `kubectl get rolebindings.rbac.authorization.k8s.io kubeadm:bootstrap-signer-clusterinfo -n kube-public -o wide`
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource (get rolebindings.rbac.authorization.k8s.io kubeadm:bootstrap-signer-clusterinfo)
- `kubectl describe rolebindings.rbac.authorization.k8s.io kubeadm:bootstrap-signer-clusterinfo -n kube-public`
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource (get rolebindings.rbac.authorization.k8s.io kubeadm:bootstrap-signer-clusterinfo)
- `kubectl get rolebindings.rbac.authorization.k8s.io system:controller:bootstrap-signer -n kube-public`
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource (get rolebindings.rbac.authorization.k8s.io system:controller:bootstrap-signer)
- `kubectl describe rolebindings.rbac.authorization.k8s.io system:controller:bootstrap-signer -n kube-public`
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource (get rolebindings.rbac.authorization.k8s.io system:controller:bootstrap-signer)
- `kubectl get roles.rbac.authorization.k8s.io -A`
  - live: NAMESPACE NAME CREATED AT | frozen: NAMESPACE NAME AGE
- `kubectl get roles.rbac.authorization.k8s.io -A -o wide`
  - live: NAMESPACE NAME CREATED AT | frozen: NAMESPACE NAME AGE
- `kubectl get roles.rbac.authorization.k8s.io kubeadm:bootstrap-signer-clusterinfo -n kube-public`
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource (get roles.rbac.authorization.k8s.io kubeadm:bootstrap-signer-clusterinfo)
- `kubectl get roles.rbac.authorization.k8s.io kubeadm:bootstrap-signer-clusterinfo -n kube-public -o wide`
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource (get roles.rbac.authorization.k8s.io kubeadm:bootstrap-signer-clusterinfo)
- `kubectl describe roles.rbac.authorization.k8s.io kubeadm:bootstrap-signer-clusterinfo -n kube-public`
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource (get roles.rbac.authorization.k8s.io kubeadm:bootstrap-signer-clusterinfo)
- `kubectl get roles.rbac.authorization.k8s.io system:controller:bootstrap-signer -n kube-public`
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource (get roles.rbac.authorization.k8s.io system:controller:bootstrap-signer)
- `kubectl describe roles.rbac.authorization.k8s.io system:controller:bootstrap-signer -n kube-public`
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource (get roles.rbac.authorization.k8s.io system:controller:bootstrap-signer)
- `kubectl get secrets bootstrap-token-abcdef -n kube-system`
  - live: NAME TYPE DATA AGE | frozen: NAME AGE
- `kubectl get secrets bootstrap-token-abcdef -n kube-system -o wide`
  - live: NAME TYPE DATA AGE | frozen: NAME AGE
- `kubectl describe secrets bootstrap-token-abcdef -n kube-system` (known)
  - exit codes live 0, frozen 1, live 0: error: illegal base64 data at input byte 8
- `kubectl get servicecidrs.networking.k8s.io -A`
  - live: NAME CIDRS AGE | frozen: NAME AGE
- `kubectl get servicecidrs.networking.k8s.io -A -o wide`
  - live: NAME CIDRS AGE | frozen: NAME AGE
- `kubectl get servicecidrs.networking.k8s.io kubernetes`
  - live: NAME CIDRS AGE | frozen: NAME AGE
- `kubectl get servicecidrs.networking.k8s.io kubernetes -o wide`
  - live: NAME CIDRS AGE | frozen: NAME AGE
- `kubectl get services -A -o wide`
  - live: NAMESPACE NAME TYPE CLUSTER-IP EXTERNAL-IP PORT(S) AGE SELECTOR | frozen: NAMESPACE NAME TYPE CLUSTER-IP EXTERNAL-IP PORT(S) AGE
- `kubectl get services cache -n shop`
  - live: NAME TYPE CLUSTER-IP EXTERNAL-IP PORT(S) AGE | frozen: NAME AGE
- `kubectl get services cache -n shop -o wide`
  - live: NAME TYPE CLUSTER-IP EXTERNAL-IP PORT(S) AGE SELECTOR | frozen: NAME AGE
- `kubectl get storageclasses.storage.k8s.io -A`
  - live: NAME PROVISIONER RECLAIMPOLICY VOLUMEBINDINGMODE ALLOWVOLUMEEXPANSION AGE | frozen: NAME AGE
- `kubectl get storageclasses.storage.k8s.io -A -o wide`
  - live: NAME PROVISIONER RECLAIMPOLICY VOLUMEBINDINGMODE ALLOWVOLUMEEXPANSION AGE | frozen: NAME AGE
- `kubectl get storageclasses.storage.k8s.io standard`
  - live: NAME PROVISIONER RECLAIMPOLICY VOLUMEBINDINGMODE ALLOWVOLUMEEXPANSION AGE | frozen: NAME AGE
- `kubectl get storageclasses.storage.k8s.io standard -o wide`
  - live: NAME PROVISIONER RECLAIMPOLICY VOLUMEBINDINGMODE ALLOWVOLUMEEXPANSION AGE | frozen: NAME AGE
- `kubectl get all -n shop`
  - live: pod/cache-9ffcf47fc-btzxm 1/1 Running 0 <age> | frozen: cache-9ffcf47fc-btzxm 1/1 Running 0 <age>
- `kubectl get all -n shop -o wide`
  - live: NAME READY STATUS RESTARTS AGE IP NODE NOMINATED NODE READINESS GATES | frozen: NAME READY STATUS RESTARTS AGE
- `kubectl get pods -n shop --sort-by=.status.startTime`
  - live: ['NAME READY STATUS RESTARTS AGE'] | frozen: ['']
- `kubectl get pods -n shop --sort-by=.metadata.name -o wide`
  - live: NAME READY STATUS RESTARTS AGE IP NODE NOMINATED NODE READINESS GATES | frozen: NAME READY STATUS RESTARTS AGE
- `kubectl get pods -n shop --sort-by=.status.containerStatuses[0].restartCount`
  - live: ['NAME READY STATUS RESTARTS AGE'] | frozen: ['']
- `kubectl get events -n shop --sort-by=.lastTimestamp`
  - live: ['LAST SEEN TYPE REASON OBJECT MESSAGE'] | frozen: ['']
- `kubectl get events -n shop --sort-by=.metadata.creationTimestamp`
  - live: LAST SEEN TYPE REASON OBJECT MESSAGE | frozen: LASTTIMESTAMP TYPE REASON OBJECT MESSAGE
- `kubectl get configmaps,secrets,persistentvolumeclaims -n shop`
  - live: configmap/kube-root-ca.crt 1 <age> | frozen: kube-root-ca.crt 1 <age>
- `kubectl get pods,services -n shop -o wide`
  - live: NAME READY STATUS RESTARTS AGE IP NODE NOMINATED NODE READINESS GATES | frozen: NAME READY STATUS RESTARTS AGE
- `kubectl rollout status deployment/checkout-api -n shop --timeout=3s`
  - live: deployment "checkout-api" successfully rolled out | frozen: deployment "cache" successfully rolled out
- `kubectl rollout status deployment/inventory-sync -n shop --timeout=3s`
  - live: deployment "inventory-sync" successfully rolled out | frozen: deployment "cache" successfully rolled out
- `kubectl get events -n shop --field-selector involvedObject.name=cache-9ffcf47fc-btzxm`
  - live: LAST SEEN TYPE REASON OBJECT MESSAGE | frozen: LASTTIMESTAMP TYPE REASON OBJECT MESSAGE
- `kubectl get pod cache-9ffcf47fc-btzxm -n shop -o wide`
  - live: NAME READY STATUS RESTARTS AGE IP NODE NOMINATED NODE READINESS GATES | frozen: NAME AGE
- `kubectl get pods -A --field-selector spec.nodeName=rcabench-worker2 -o wide`
  - live: NAMESPACE NAME READY STATUS RESTARTS AGE IP NODE NOMINATED NODE READINESS GATES | frozen: NAMESPACE NAME READY STATUS RESTARTS AGE
- `kubectl get events -n shop --field-selector involvedObject.name=checkout-api-58f9d6b876-8m9f4`
  - live: LAST SEEN TYPE REASON OBJECT MESSAGE | frozen: LASTTIMESTAMP TYPE REASON OBJECT MESSAGE
- `kubectl get pod checkout-api-58f9d6b876-8m9f4 -n shop -o wide`
  - live: NAME READY STATUS RESTARTS AGE IP NODE NOMINATED NODE READINESS GATES | frozen: NAME AGE
- `kubectl get pods -A --field-selector spec.nodeName=rcabench-worker -o wide`
  - live: NAMESPACE NAME READY STATUS RESTARTS AGE IP NODE NOMINATED NODE READINESS GATES | frozen: NAMESPACE NAME READY STATUS RESTARTS AGE
- `kubectl cluster-info` (known)
  - live: Kubernetes control plane is running at https://127.0.0.1:54015 | frozen: Kubernetes control plane is running at http://127.0.0.1:55380/kubernetes
- `kubectl explain pods` (known)
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource
- `kubectl explain deployment.spec.strategy` (known)
  - exit codes live 0, frozen 1, live 0: Error from server (NotFound): the server could not find the requested resource
- `kubectl get pods -n shop -l app=report-worker -o wide`
  - live: NAME READY STATUS RESTARTS AGE IP NODE NOMINATED NODE READINESS GATES | frozen: NAME READY STATUS RESTARTS AGE
- `kubectl get events -n shop --sort-by=.lastTimestamp -o wide`
  - live: ['LAST SEEN TYPE REASON OBJECT SUBOBJECT SOURCE MESSAGE FIRST SEEN COUNT NAME'] | frozen: ['']
- `kubectl get endpoints cache -n shop -o yaml`
  - stderr, live: Warning: v1 Endpoints is deprecated in v1.33+; use discovery.k8s.io/v1 EndpointSlice | frozen: (nothing)
- `kubectl get endpointslices -n shop -o wide`
  - live: NAME ADDRESSTYPE PORTS ENDPOINTS AGE | frozen: NAME AGE

…and 31 more of the same kinds.

### Both fail, worded differently

- `kubectl logs -n shop report-worker-9c8dbb6dd-wjm9j --tail=30`
  - live: error: error from server (NotFound): pods "report-worker-9c8dbb6dd-wjm9j" not found in namespace "shop" | frozen: error: error from server (NotFound): the server could not find the requested resource (get pods report-worker-9c8dbb6dd-wjm9j) in namespace "shop"
- `kubectl get pods -n shop report-worker-* -o wide`
  - live: Error from server (NotFound): pods "report-worker-*" not found | frozen: Error from server (NotFound): the server could not find the requested resource (get pods report-worker-*)
- `kubectl get events -n shop --sort-by=.lastTimestamp --field-selector lastTimestamp`
  - live: Error from server (BadRequest): Unable to find "/v1, Resource=events" that match label selector "", field selector "lastTimestamp": invalid selector:  | frozen: Error from server (BadRequest): Unable to find "/v1, Resource=events" that match label selector "", field selector "lastTimestamp": invalid field sele

Differing and not listed as known: 178.
