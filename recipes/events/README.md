# Alertmanager's event recorder, from a file or Kafka

Alertmanager's event recorder (`--enable-feature=event-recorder`, 0.33.0
on) writes every delivery an integration made, and every mute, to its
outputs. `lapilli serve` takes them on its notifications port, at
`/v1/alertmanager/events`, as a `webhook_outputs` entry posts them
([Notifications as they were sent](../../docs/notifications.md)). Where the
recorder writes to a file or to Kafka instead, `alertmanager.sh` sends what
it wrote on, in batches, and resumes where it stopped.

```yaml
event_recorder:
  file_outputs:
    - name: file
      path: /alertmanager/events.jsonl   # one event a line
  kafka_outputs:
    - name: kafka
      brokers: [kafka-0:9092]
      topic: alertmanager-events
      format: json                       # the default; protobuf isn't read
```

| Run | Reads | Resumes from |
|---|---|---|
| `alertmanager.sh file PATH` | the file, from where it stopped | `OFFSET_FILE` (default `PATH.offset`): the file's inode, a byte offset into it, and a sum of its first bytes |
| `alertmanager.sh kafka` | the topic, a partition at a time, with `kcat -J`, each message's offset from its envelope | `OFFSET_FILE`: a line a partition, its next offset |
| `KAFKA_GROUP=NAME alertmanager.sh kafka` | the topic, as a member of the consumer group | the offsets Kafka keeps for the group, which `kcat` commits once it has read the messages |

```sh
export LAPILLI_URL=http://lapilli-notify:8083 LAPILLI_TOKEN_FILE=/etc/lapilli/notify-token
./alertmanager.sh file /alertmanager/events.jsonl                       # once, from cron
FOLLOW=true ./alertmanager.sh file /alertmanager/events.jsonl           # beside Alertmanager, sharing its volume
KAFKA_BROKERS=kafka-0:9092 KAFKA_TOPIC=alertmanager-events OFFSET_FILE=/var/lib/lapilli/kafka.offsets ./alertmanager.sh kafka
```

It needs `sh`, `jq` and `curl`; `kcat` for Kafka (`KCAT_CONFIG` names a
file of librdkafka's settings, for TLS or SASL). It only reads the file or
the topic.

- **At least once.** An offset is kept after each batch serve took. One
  sent again is the same delivery to serve, which keys it by the
  instance, the group, the flush and the receiver: kept once. A batch
  serve didn't take (it was down, or refused the token) stops the run,
  which exits 1, and is sent again the next time. One serve answers 400,
  events it can't read as the recorder's, is passed over and said: sent
  again, it would be refused again. One answered 413, or whose body is
  over 1 MiB as `jq` writes it, is sent an event at a time, and an event
  serve can't take alone is passed over, and said. A consumer group's offsets are
  committed by `kcat` once it has read the messages, so a run stopped
  before it sent them doesn't read them again: keep offsets in a file for
  at least once.
- **Rotated or truncated.** Alertmanager doesn't rotate its file; it
  reopens it when logrotate renames it. A file renamed beside the path
  (`PATH.1`, as logrotate's `delaycompress` leaves it) is read to its end
  first, then the new file from its start. A file truncated in place
  (`copytruncate`), or written again from its start, is told by its size
  or by its first bytes, and read again from its start. A line the
  recorder is still writing waits for its end.
- **Bounded.** `BATCH` events a request (default 100, 1,000 at most), and
  under 1 MB, as serve takes them; an event alone over that is passed
  over, and said. 16 MiB of the file are read at a time; 10,000 messages
  of a partition each run. `curl` tries again on 429 and 5xx, as
  `Retry-After` says, `RETRIES` times (default 8). A line that isn't a
  JSON object, such as the Kafka output's `format: protobuf`, isn't sent,
  and is counted.
- **The time.** serve takes an event's own `@timestamp` when it is within
  the hour before it arrives, and the time it arrives otherwise. Run it
  often: every minute from cron, or `FOLLOW=true` (every `POLL` seconds,
  default 5) as a sidecar. A first read of Kafka with no offsets begins
  an hour ago (`KAFKA_START=beginning` or `end` for the topic's start or
  end); a consumer group with none begins at the end.
- **The token** is read as the other recipes read theirs: one line, no
  control character, spaces after it dropped, sent to `curl` on stdin so
  `ps` doesn't show it. `LAPILLI_URL` is http or https, with no user
  before `@`, query or anchor, and `curl` speaks no other scheme. Over
  plain http the token is read by whatever is on the way: keep that to
  the cluster serve runs in, as its port is. `SKIP` names receivers to
  pass over, as serve's `?skip=`: Lapilli's own webhook receiver, while
  it is kept beside the recorder.

Keep `OFFSET_FILE` where the script can write, not on a read-only mount
beside Alertmanager's file: where it can't keep where it stopped, it says so
and stops, exiting 1, rather than send the same again each run, as a
delivery is kept once but a mute, which has no key of its own, isn't.

Run one at a time for an offset file: two would each send what the other
sent, which serve keeps once, but each move the offset as it read.
