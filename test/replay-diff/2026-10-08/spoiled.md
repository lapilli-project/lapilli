| the frozen answer, spoiled | still passes | of |
|---|---|---|
| replaced by nothing | 0 | 1157 |
| cut to its first line | 0 | 1157 |
| its last line taken off | 16 | 1157 |
| its last line written twice | 15 | 1157 |
| its lines the other way up | 38 | 1149 |

its last line taken off, and still passing:

- `[same] kubectl logs report-worker-9c8dbb6dd-2d7pj -n shop --tail=20`
- `[same] kubectl logs report-worker-9c8dbb6dd-2d7pj -n shop`
- `[same] kubectl logs report-worker-9c8dbb6dd-2d7pj -n shop --since-time=2026-10-07T18:08:48Z`
- `[same] kubectl logs report-worker-9c8dbb6dd-2d7pj -n shop --since=45s`
- `[same] kubectl logs report-worker-9c8dbb6dd-96wkn -n shop --tail=20`
- `[same] kubectl logs report-worker-9c8dbb6dd-96wkn -n shop`
- `[same] kubectl logs ledger-api-866db84748-4l66v -n ledger --tail=20`
- `[same] kubectl logs ledger-api-866db84748-4l66v -n ledger`
- and 8 more

its last line written twice, and still passing:

- `[moved] kubectl logs cache-9ffcf47fc-wb75z -n shop --tail=5 --all-containers`
- `[moved] kubectl logs checkout-api-58f9d6b876-bz5jp -n shop --tail=5 --all-containers`
- `[moved] kubectl logs checkout-api-58f9d6b876-tftl8 -n shop --tail=5 --all-containers`
- `[moved] kubectl logs inventory-sync-7fd8b9d79-nqjm9 -n shop --tail=5 --all-containers`
- `[moved] kubectl logs report-worker-9c8dbb6dd-2d7pj -n shop --tail=5 --all-containers`
- `[moved] kubectl logs report-worker-9c8dbb6dd-96wkn -n shop --tail=5 --all-containers`
- `[moved] kubectl logs report-worker-9c8dbb6dd-m599j -n shop --tail=5 --all-containers`
- `[moved] kubectl logs image-proxy-6c497c8656-vn4zr -n media --tail=5 --all-containers`
- and 7 more

its lines the other way up, and still passing:

- `[order] kubectl get pods -n shop --sort-by=.metadata.name -o wide`
- `[moved] kubectl logs deployment/checkout-api -n shop --tail=3`
- `[moved] kubectl logs cache-9ffcf47fc-wb75z -n shop --tail=5 --all-containers`
- `[moved] kubectl logs checkout-api-58f9d6b876-bz5jp -n shop --tail=5 --all-containers`
- `[moved] kubectl logs checkout-api-58f9d6b876-bz5jp -n shop --timestamps --tail=3`
- `[moved] kubectl logs checkout-api-58f9d6b876-tftl8 -n shop --tail=5 --all-containers`
- `[moved] kubectl logs checkout-api-58f9d6b876-tftl8 -n shop --timestamps --tail=3`
- `[moved] kubectl logs inventory-sync-7fd8b9d79-nqjm9 -n shop --tail=5 --all-containers`
- and 30 more
