| the frozen answer, spoiled | still passes | of |
|---|---|---|
| replaced by nothing | 0 | 1325 |
| cut to its first line | 1 | 1325 |
| its last line taken off | 39 | 1325 |
| its last line written twice | 17 | 1325 |
| its lines the other way up | 47 | 1319 |
| its first line moved to its end | 90 | 1319 |

cut to its first line, and still passing:

- `[moved] kubectl logs image-proxy-6c497c8656-qvl56 -n media --tail=5 --all-containers`

its last line taken off, and still passing:

- `[same] kubectl logs checkout-api-58f9d6b876-hqd8m -n shop --tail=20`
- `[same] kubectl logs checkout-api-58f9d6b876-hqd8m -n shop`
- `[same] kubectl logs checkout-api-58f9d6b876-hqd8m -n shop --since-time=2026-10-08T05:08:57Z`
- `[same] kubectl logs checkout-api-58f9d6b876-hqd8m -n shop --since=45s`
- `[same] kubectl logs checkout-api-58f9d6b876-qn9b2 -n shop --tail=20`
- `[same] kubectl logs checkout-api-58f9d6b876-qn9b2 -n shop`
- `[same] kubectl logs checkout-api-58f9d6b876-qn9b2 -n shop --since-time=2026-10-08T05:08:57Z`
- `[same] kubectl logs checkout-api-58f9d6b876-qn9b2 -n shop --since=45s`
- and 31 more

its last line written twice, and still passing:

- `[moved] kubectl logs cache-9ffcf47fc-5bc6f -n shop --tail=5 --all-containers`
- `[moved] kubectl logs checkout-api-58f9d6b876-hqd8m -n shop --tail=5 --all-containers`
- `[moved] kubectl logs checkout-api-58f9d6b876-qn9b2 -n shop --tail=5 --all-containers`
- `[moved] kubectl logs inventory-sync-7fd8b9d79-q855c -n shop --tail=5 --all-containers`
- `[moved] kubectl logs report-worker-9c8dbb6dd-5xm6w -n shop --tail=5 --all-containers`
- `[moved] kubectl logs report-worker-9c8dbb6dd-gk9qs -n shop --tail=5 --all-containers`
- `[moved] kubectl logs report-worker-9c8dbb6dd-nn9tm -n shop --tail=5 --all-containers`
- `[moved] kubectl logs -n shop deploy/report-worker --tail=5 --all-containers --prefix`
- and 9 more

its lines the other way up, and still passing:

- `[order] kubectl get pods -n shop --sort-by=.metadata.name -o wide`
- `[order] kubectl get events -n shop --sort-by=.metadata.creationTimestamp`
- `[moved] kubectl logs deployment/checkout-api -n shop --tail=3`
- `[moved] kubectl logs deployment/report-worker -n shop --tail=3`
- `[moved] kubectl logs cache-9ffcf47fc-5bc6f -n shop --tail=5 --all-containers`
- `[moved] kubectl logs checkout-api-58f9d6b876-hqd8m -n shop --tail=5 --all-containers`
- `[moved] kubectl logs checkout-api-58f9d6b876-hqd8m -n shop --timestamps --tail=3`
- `[moved] kubectl logs checkout-api-58f9d6b876-qn9b2 -n shop --timestamps --tail=3`
- and 39 more

its first line moved to its end, and still passing:

- `[order] kubectl get pods -n shop --sort-by=.status.startTime`
- `[order] kubectl get pods -n shop --sort-by=.metadata.name -o wide`
- `[order] kubectl get pods -n shop --sort-by=.status.containerStatuses[0].restartCount`
- `[order] kubectl get events -n shop --sort-by=.lastTimestamp`
- `[order] kubectl get events -n shop --sort-by=.metadata.creationTimestamp`
- `[order] kubectl events -n shop`
- `[moved] kubectl logs deployment/checkout-api -n shop --tail=3`
- `[moved] kubectl logs deployment/report-worker -n shop --tail=3`
- and 82 more
