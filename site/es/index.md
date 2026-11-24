---
layout: default
home: true
lang: es
permalink: /es/
title: ¿Cuáles de tus alertas merecen avisar a alguien?
description: "Lapilli juzga cada disparo frente a lo que de verdad salió mal: una piedra blanca cuando detectó un incidente, una negra cuando no pasaba nada. Una vez con lapilli verdict, o cada hora con lapilli serve."
---

<p class="tags"><span class="tag">Apache-2.0 · código abierto</span>{% if site.version %}
<a href="{{ site.repo }}/releases/tag/{{ site.version }}">Ya está disponible Lapilli {{ site.version }}: novedades</a>{% endif %}</p>

# ¿Cuáles de tus alertas merecen avisar a alguien?

<p class="lede">Lapilli juzga cada disparo frente a lo que de verdad salió mal:
una piedra blanca cuando la alerta detectó un incidente, una piedra negra cuando
no pasaba nada. Cuenta las piedras de una regla y sabrás si se gana su sitio; un
incidente sin piedra es uno que tus alertas no detectaron.</p>

<div class="cta">
  <a class="primary" href="{{ '/docs/getting-started/' | relative_url }}">Empezar <span aria-hidden="true">→</span></a>
  <a href="{{ '/docs/' | relative_url }}">Leer la documentación</a>
  <a href="#help-build-it">Ayuda a construirlo</a>
</div>

```sh
go install lapilli.dev/cmd/lapilli@latest
lapilli demo
```

<p class="hint"><code>lapilli demo</code> juzga una semana inventada de alertas de una
pequeña tienda, sin nada que conectar. O bien <a href="#install">un binario, la
imagen o el chart de Helm</a>.</p>

<div class="shot">
<picture>
  <source media="(prefers-color-scheme: dark)" srcset="{{ '/serve-dashboard-dark.png' | relative_url }}">
  <img alt="El resumen general de lapilli serve, con sus páginas en una barra lateral: la precisión de la semana pasada, los disparos, los incidentes sin detectar, el tiempo hasta detectar (ninguno), las reglas ruidosas, las notificaciones por disparos que no detectaron nada mientras la gente dormía, los redisparos y los disparos durante despliegues, cada uno con una línea de sus semanas y la mayoría junto a la semana anterior; debajo, qué revisar: reglas a las que reducir el ruido o que revisar, cada una con un botón para convertirla en tarea" src="{{ '/serve-dashboard.png' | relative_url }}" width="1240" height="980">
</picture>
</div>

<ul class="facts">
  <li><b>Lee</b> Prometheus y lo que hable su API, Grafana, Loki, las alarmas propias de una nube</li>
  <li><b>Se ejecuta</b> una vez con <code>lapilli verdict</code>, cada hora con <code>lapilli serve</code></li>
  <li><b>Guarda</b> un solo archivo SQLite, o una base de datos Postgres que comparten varias réplicas</li>
  <li><b>Cambia</b> solo lo que aprueba una persona, y por defecto solo lee</li>
</ul>

Trabaja con lo que ya tienes: las reglas, el historial de Prometheus y lo que
tengas anotado sobre incidentes, aunque sea poco. Por defecto solo lee, y cambia
únicamente lo que aprueba una persona.

Está en sus inicios, con un mantenedor y sitio para más. Probado con un Grafana
de producción: el backtest reproduce sus 71 reglas de alerta, y uno de cuatro
semanas, con las reglas tal como estaban entonces, encontró 177 disparos de la
regla más ruidosa: el número de notificaciones que su equipo había contado a mano.

## La alerta saltó. ¿Y después? {#the-alert-fired-then-what}

Una alerta dice que algo cruzó un umbral. No dice si avisar a alguien fue lo correcto. Quedan tres preguntas:

<div class="grid">
  <div><h3>¿Pasaba algo de verdad?</h3><p>Un aviso a las tres de la madrugada, y nada que arreglar. ¿Fue una vez, o es lo que hace esta regla? Lapilli cuenta, regla por regla, los disparos que detectaron un incidente y los que no detectaron nada.</p></div>
  <div><h3>¿Lo detectó alguna alerta?</h3><p>Un cliente encontró el incidente antes que ninguna alerta. Lapilli muestra los incidentes que ninguna regla detectó, y las reglas que casi lo hicieron: la condición se cumplió, pero no el tiempo suficiente, o se quedó justo por debajo del umbral.</p></div>
  <div><h3>¿Qué hará este cambio?</h3><p>Un <code>for</code> más largo, un umbral nuevo, una regla retirada: se fusiona a ojo y se juzga en el próximo incidente. Lapilli hace un backtest del cambio sobre las semanas pasadas, antes de fusionarlo.</p></div>
</div>

## Una vez: `lapilli verdict` {#once-lapilli-verdict}

Haz un backtest de una semana, de un mes o de un cambio que estás a punto de
fusionar, y obtén las piedras, los disparos que despertaron a alguien en vano,
las reglas ruidosas con el `for` más largo que reduciría su ruido, los incidentes
que nada detectó y las reglas que casi los detectaron.

```
$ lapilli verdict --rules alert-rules.yaml --prometheus http://prometheus:9090 \
    --incidents incidents.yaml --impact 'sum(rate(http_5xx[5m])) / sum(rate(http_total[5m])) > 0.01'
Lapilli  2026-10-26 00:00 → 2026-11-02 00:00 Asia/Seoul (+09:00)
148 firings, 3 incidents written down, 1 more found by --impact

RULE                SEVERITY  FIRED  ○ CAUGHT  ● NOTHING  PRECISION  REFIRED
ExportJob5xx        critical  118    0         118        0%         41
CheckoutLatencyP99  warning   14     2         12         14%        3
ReadReplicaLag      warning   9      1         8          11%        0
NodeNotReady        critical  4      0         4          0%         0
DBPrimaryCPU        critical  3      2         1          67%        0

Noisy rules that a longer "for" would quiet, still catching every incident they caught:
RULE                FOR       FIRED     INCIDENTS  CAUGHT LATER BY
ExportJob5xx        5m → 15m  118 → 31  0          –
CheckoutLatencyP99  5m → 10m  14 → 6    1          up to 4m

Out of hours, in Asia/Seoul (at work mon-fri 08:00-18:00, asleep 22:00-08:00):
of the firings that caught nothing, 57 came while people slept, and 37 more off hours.
RULE                SEVERITY  ● ASLEEP  ● OFF HOURS  ● AT WORK
ExportJob5xx        critical  52        31           35
NodeNotReady        critical  3         0            1
ReadReplicaLag      warning   2         1            5
CheckoutLatencyP99  warning   0         4            8
DBPrimaryCPU        critical  0         1            0

What each rule adds: the incidents it caught, those it caught before any other rule, those no other rule caught:
RULE                INCIDENTS  FIRST  ALONE
CheckoutLatencyP99  1          0      0
DBPrimaryCPU        1          1      1
ReadReplicaLag      1          1      1
A noisy rule's catch doesn't count beside one by a rule that isn't noisy.

38 rules never fired.

INCIDENT                         START             LASTED  CAUGHT BY
INC-41 Checkout errors           2026-10-28 12:01  38m     DBPrimaryCPU +2m, CheckoutLatencyP99 +6m (noisy)
INC-42 Search timeouts           2026-10-30 09:15  1h12m   ReadReplicaLag -3m
INC-43 Batch stuck               2026-10-31 02:40  –       ✗ nothing fired
impact-1 user impact (--impact)  2026-11-01 21:27  24m     ✗ nothing fired

✗ 2 of 4 incidents had no alert.
! --impact-for 5m counts impact that lasted 5m as an incident, but 9 of the 43 rules judged fire only once
  their condition held longer: their "for" and one evaluation, with no "for" an evaluation (6m for 6
  of them). Impact shorter than that they can't be sure to catch. --impact-for 6m counts only what they can.

Rules that nearly caught them: their condition held, but not for their "for", or fired too late; or their value came within a fifth of their threshold.
INCIDENT  RULE             HELD          CHANGE         WOULD FIRE  FIRED  ● NOTHING
INC-43    BatchQueueDepth  20m from -5m  for 30m → 19m  +14m        0 → 3  0 → 2
impact-1  CheckoutLatency  reached 0.86  > 0.9 → 0.85   +6m         0 → 2  0 → 1
```

`--compare` hace el backtest de las reglas vigentes junto al de las propuestas,
sobre el mismo periodo, y muestra las piedras de cada regla y las detecciones de
cada incidente antes → después; sin incidentes, cuántas veces se dispara cada una.
`--expect` exige a las reglas que cambian lo que se espera de ellas: cuántas veces
puede dispararse cada una en una semana, y mientras la gente duerme, y los
incidentes que debe seguir detectando. Ejecútalo en CI en un pull request que
toque las reglas: un script para GitHub, GitLab, Bitbucket, Azure DevOps y Gitea,
y una action para GitHub Actions, publican el informe en Markdown como comentario
del pull request y mantienen al día ese único comentario. El informe también sale
en JSON y como métricas de Prometheus, y se envía a Slack y a un Pushgateway.

`--check-replay` contrasta el backtest con lo que realmente se disparó, regla por
regla, tal como lo guardaron Prometheus, el historial de estados de alerta del
propio Grafana o las alarmas de una nube, para que puedas comprobar el backtest
con tus propias reglas antes de fiarte de un veredicto. `--check-content` revisa
lo que dice cada alerta cuando se dispara, y `--redact` da un alias a cada
nombre, para un informe que vaya a mostrarse fuera.

## Cada hora: `lapilli serve` {#every-hour-lapilli-serve}

`lapilli serve` lleva un registro: cada disparo, cada incidente y cada versión de
las reglas, con un backtest cada hora con las reglas tal como estaban. Muestra las
piedras en una UI web donde los equipos actúan sobre ellas. Antes de desplegarlo,
`lapilli preflight` toma sus flags y dice con qué se va a encontrar: las reglas a
las que puede hacer backtest, el historial y el inicio de sesión en él, el
registro, y las peticiones y la memoria que necesitarán sus ejecuciones. Solo
lee; no escribe nada.

<div class="tour">
<figure>
<picture>
  <source media="(prefers-color-scheme: dark)" srcset="{{ '/serve-rules-dark.png' | relative_url }}">
  <img alt="La lista de reglas de lapilli serve, filtrada a las reglas ruidosas de cuatro semanas: cada regla con sus insignias, sus etiquetas, su equipo y sus cifras" src="{{ '/serve-rules.png' | relative_url }}" class="end" width="1240" height="1600" loading="lazy">
</picture>
<figcaption><h3>Encuentra las reglas que más cuestan</h3><p>La lista de reglas, filtrada a las ruidosas: cada una con sus piedras, el <code>for</code> que la calmaría y los disparos que llegaron mientras la gente dormía.</p><p class="go"><a href="{{ '/docs/serve/#rules-and-proposals' | relative_url }}">La lista de reglas, en la documentación (en inglés)</a></p></figcaption>
</figure>
<figure>
<picture>
  <source media="(prefers-color-scheme: dark)" srcset="{{ '/serve-rule-dark.png' | relative_url }}">
  <img alt="La página de una regla ruidosa en lapilli serve: sus insignias y acciones, cinco cifras junto a las cuatro semanas anteriores y sus últimos 84 días como una tira de celdas" src="{{ '/serve-rule.png' | relative_url }}" width="1240" height="980" loading="lazy">
</picture>
<figcaption><h3>Lee una regla, semana a semana</h3><p>La página de una regla: su precisión y sus disparos junto al periodo anterior, sus últimos 84 días, una celda por día, cada versión y qué hacer primero. Un cambio se propone con su backtest y lo aprueba una persona.</p><p class="go"><a href="{{ '/docs/changing-rules/' | relative_url }}">Cambiar reglas, en la documentación (en inglés)</a></p></figcaption>
</figure>
</div>

<div class="grid">
  <div><h3>Un dashboard</h3><p>La precisión de la semana pasada, los disparos, los incidentes sin detectar, las reglas ruidosas y los disparos que despertaron a alguien en vano, junto a la semana anterior; doce semanas, semana a semana; dónde está el ruido, por regla y semana; qué revisar primero.</p></div>
  <div><h3>Backtest de cada cambio</h3><p>A cada nueva versión de las reglas se le hace un backtest sobre la semana anterior, para que veas qué habría hecho el cambio.</p></div>
  <div><h3>Incidentes registrados</h3><p>Anota un incidente, o indica que una piedra negra fue real, y todo el registro se vuelve a juzgar al instante, sin preguntar a Prometheus.</p></div>
  <div><h3>Ventanas de mantenimiento</h3><p>Un despliegue, un mantenimiento, un batch nocturno: los disparos de una ventana que no detectaron nada quedan eximidos, una sola vez o cada día o cada semana.</p></div>
  <div><h3>Ventanas marcadas</h3><p>Unas rebajas, un lanzamiento, una campaña: se definen en un archivo, en la UI o con una expresión PromQL. No se exime nada; cada recuento se lee dentro y fuera de ellas, y la precisión durante cada una, junto a la de las cuatro semanas anteriores.</p></div>
  <div><h3>Objetivos</h3><p>Lo que un equipo exige a sus alertas: un límite de precisión, de incidentes sin detectar, de disparos críticos o de redisparos, semana a semana o en cada periodo de rebajas. Un objetivo incumplido se notifica y, con un clic, se convierte en tarea.</p></div>
  <div><h3>Señales de impacto en usuarios</h3><p>Nombra las señales que dicen que los usuarios están sufriendo, como una tasa de errores o una latencia, y las ventanas en las que se cumple alguna también son incidentes; los disparos de cada semana se juzgan con las señales vinculadas a ellos.</p></div>
  <div><h3>Equipos, tareas, comentarios</h3><p>Los equipos son dueños de reglas e incidentes; con un clic, una sugerencia se convierte en tarea, con responsable, fecha de vencimiento y un enlace a tu tracker.</p></div>
  <div><h3>Incidentes desde donde se guardan</h3><p>Unos scripts los copian desde PagerDuty, incident.io, Jira e issues de GitHub a un puerto que acepta incidentes con un token y no hace nada más. Si se vuelven a escribir sin cambios, no pasa nada.</p></div>
  <div><h3>Las alarmas de una nube, conservadas</h3><p>Un script lee el historial de alarmas de CloudWatch, Azure Monitor, Cloud Monitoring u OCI y lo copia, con su propio token, para que el registro conserve más de lo que conserva la nube; sin reglas propias, lapilli serve funciona solo con eso. Si además se leen a través de la API de la nube, una alarma y su historial son una sola regla.</p></div>
  <div><h3>Notificaciones, no solo disparos</h3><p>Las notificaciones tal como las enviaron Alertmanager, Grafana, SNS, Azure Monitor y Cloud Monitoring, en un puerto propio: las notificaciones de cada regla, sus repeticiones y las que llegaron mientras la gente dormía, junto a sus disparos.</p></div>
  <div><h3>Avisos y resúmenes</h3><p>Lo que pasa se envía a un webhook en JSON, y a Slack, a través de una bandeja de salida que se escribe junto con el cambio. Lo mismo ocurre con el resumen de cada semana, y cada equipo puede tener el suyo.</p></div>
  <div><h3>Declarado en Git</h3><p>Equipos, ventanas de mantenimiento y marcadas, objetivos, informes y roles en un solo archivo, que se aplica a medida que cambia, sin reiniciar; lo que declara es de solo lectura en la UI.</p></div>
  <div><h3>Roles</h3><p>Lectores, editores y administradores, que inician sesión con Lapilli mediante cualquier proveedor de OpenID Connect, o con el inicio de sesión que tenga delante: el identity-aware proxy de una nube, el de un balanceador de carga, oauth2-proxy.</p></div>
  <div><h3>Resultados</h3><p>Cada cambio en las reglas, con su predicción junto a lo que de verdad pasó; si le siguen llegando incidentes; si se usa. Sale del registro y no se envía a ninguna parte.</p></div>
  <div><h3>Cambios con aprobación</h3><p>Un for más largo, un umbral más cercano, una regla redundante retirada, un enlace al runbook que escribió una persona: cada uno se propone con su backtest, lo aprueba una persona en su página, se vuelve a comprobar y se abre como pull request en GitHub, GitLab, Bitbucket, Gitea o Azure DevOps mediante un script que tiene las credenciales del repositorio, o se aplica a través de la API de Grafana, la de un ruler de Mimir, Cortex, Amazon o Azure, o la de las alarmas propias de una nube (CloudWatch, Azure Monitor, Cloud Monitoring, OCI), allí donde se le permita hacer cambios. Desactivado hasta que lo actives; un interruptor de parada y límites; no se desactiva nada mientras se dispara.</p></div>
  <div><h3>Agentes, por MCP</h3><p>Un agente consulta lo que muestran las páginas, hace un backtest del cambio que propone en las reglas antes de que nadie lo aplique y, si se le permite, lo guarda como propuesta para que una persona la apruebe. Nunca aprueba ni aplica ninguna.</p></div>
  <div><h3>Inglés, español, japonés y coreano</h3><p>Un archivo de traducciones por idioma; un test falla si a un texto le falta su traducción.</p></div>
</div>

El registro es un solo archivo SQLite, o una base de datos Postgres que comparten
varias réplicas, una de ellas como líder. Los tests limitan una ejecución horaria
sobre doce semanas de 200.000 disparos a dos segundos, y cada página a unas pocas
consultas por regla, nunca una por disparo. `/metrics` entrega las piedras a Prometheus,
y el chart de Helm, un dashboard de Grafana con ellas. La UI no ejecuta scripts.

## Cómo juzga {#how-it-judges}

- **Los disparos** salen de hacer backtest de tus reglas de alerta contra el
  historial. Las reglas pueden ser archivos de reglas de Prometheus, `PrometheusRule`,
  el provisioning de alertas de Grafana, los grupos de reglas gestionados de una
  nube, sus alarmas escritas en PromQL (alarmas PromQL de CloudWatch, políticas de
  alertas de Google Cloud) o ConfigMaps que las contengan, cada versión mientras
  estuvo vigente, y las reglas en LogQL de un ruler de Loki. También pueden ser las
  reglas que tiene cargadas un Prometheus o un Grafana. Los disparos también pueden
  venir de la serie `ALERTS`, o de un archivo o un job que los copie, como el
  historial de alarmas de una nube. Las reglas de Grafana se evalúan como lo hace
  Grafana.
- **Los incidentes** vienen de un archivo YAML, de la UI y la API, de PagerDuty,
  incident.io, Jira o issues de GitHub mediante un script, y de las ventanas en las
  que se cumple una señal de impacto en usuarios (`--impact`). Aunque tengas pocos
  incidentes anotados, funciona.
- **Un disparo detecta un incidente** cuando empieza poco antes o durante él y
  coincide en las etiquetas que indiques. Uno que ya se estaba disparando antes no
  detectó nada: estaba activo de todos modos.
- **Sin registro, no hay veredicto.** Un disparo que no detectó nada solo se juzga
  si había algo anotado a su alrededor: un incidente en las dos semanas anteriores,
  o una señal de impacto leída durante su tiempo. Si no, se cuenta, pero no se
  juzga, y el informe lo dice.

El resto (las reglas ruidosas, qué cambiar y las casi detecciones) está en
[Cómo juzga]({{ '/docs/how-it-judges/' | relative_url }}).

## Funciona donde están tus alertas {#works-where-your-alerts-are}

Sirve cualquier API compatible con Prometheus: Prometheus, Thanos, Mimir,
VictoriaMetrics, el Prometheus gestionado de AWS, Azure o Google Cloud, o la API
PromQL de CloudWatch, con las credenciales que ya tiene la máquina o el pod.
Las reglas sobre logs se someten a backtest contra Loki
([Reglas sobre logs]({{ '/docs/loki/' | relative_url }})).
Oracle Cloud no tiene Prometheus gestionado; en OKE, usa el que ejecutes allí.
Las alarmas en los términos propios de una nube (CloudWatch, Azure Monitor, Cloud
Monitoring, OCI Monitoring) se juzgan a partir de su historial, con un pequeño
script para cada nube, y `lapilli serve` conserva ese historial más allá de lo que
lo conserva la nube. Las integraciones son genéricas: un webhook, no un tracker
concreto.

## Empieza con lo que ya tienes {#start-from-what-you-have}

No hay que mover ni reescribir nada antes. Cada cosa de aquí es un script de <a href="{{ site.repo }}/tree/main/recipes">recipes</a>, en el repositorio, o unos pocos flags.

<div class="grid">
  <div><h3>Reglas en un repositorio</h3><p>Un pull request que cambia reglas de alerta recibe su backtest como comentario, y un estado que puede impedir la fusión. Para GitHub, GitLab, Bitbucket, Azure DevOps y Gitea.</p><p class="go"><a href="{{ '/docs/pr-backtests/' | relative_url }}">Pull request backtests (en inglés)</a> · <a href="{{ site.repo }}/tree/main/recipes/backtest"><code>recipes/backtest</code></a></p></div>
  <div><h3>Incidentes en una herramienta</h3><p>PagerDuty, incident.io, FireHydrant, Rootly, Grafana IRM, Compass, Jira o issues de GitHub: un script los copia, con las alertas que la herramienta enlazó a cada uno.</p><p class="go"><a href="{{ '/docs/incidents/' | relative_url }}">Incidents kept elsewhere (en inglés)</a> · <a href="{{ site.repo }}/tree/main/recipes/incidents"><code>recipes/incidents</code></a></p></div>
  <div><h3>El historial de alarmas de una nube</h3><p>Las alarmas de CloudWatch, Azure Monitor, Cloud Monitoring y OCI, los monitores de Datadog y las condiciones de alerta de New Relic, juzgados por lo que dispararon.</p><p class="go"><a href="{{ '/docs/cloud-alarms/' | relative_url }}">A cloud's own alarms (en inglés)</a> · <a href="{{ site.repo }}/tree/main/recipes/firings"><code>recipes/firings</code></a></p></div>
  <div><h3>Las reglas como eran</h3><p>Versiones anteriores de las reglas desde git, Argo CD o Flux, para que cada semana tenga su backtest con las reglas que tenía.</p><p class="go"><a href="{{ '/docs/how-it-judges/#rules-that-changed' | relative_url }}">Rules that changed (en inglés)</a> · <a href="{{ site.repo }}/tree/main/recipes/versions"><code>recipes/versions</code></a></p></div>
  <div><h3>Cambios como pull requests</h3><p>Un cambio aprobado se abre como pull request donde se guarda la regla.</p><p class="go"><a href="{{ '/docs/changing-rules/#carrying-out-a-change' | relative_url }}">Changing rules (en inglés)</a> · <a href="{{ site.repo }}/tree/main/recipes/changes"><code>recipes/changes</code></a></p></div>
  <div><h3>Tareas en tu tracker</h3><p>Jira, Linear, GitHub, GitLab, Azure Boards, Shortcut y YouTrack, al día con las tareas de Lapilli.</p><p class="go"><a href="{{ '/docs/serve/#tasks-kept-in-step-with-a-tracker' | relative_url }}">Tasks kept in step with a tracker (en inglés)</a> · <a href="{{ site.repo }}/tree/main/recipes/trackers"><code>recipes/trackers</code></a></p></div>
</div>

## Hecho para que puedas fiarte {#built-to-be-trusted}

- **Solo cambia lo que aprueba una persona.** De serie, solo lee. Si lo activas,
  aplica los cambios en las reglas que propone, cada uno una vez que una persona
  lo ha aprobado en la UI: como pull request allí donde se guarda la regla, o a
  través de la API de Grafana, de un ruler o de una nube, guardando lo que
  reemplaza para poder deshacerlo. Nunca silencia una alerta, nunca desactiva una
  mientras se dispara, y un agente solo puede proponer.
- **Los secretos se quedan donde están.** Los tokens y las URL de webhook se leen
  de archivos y nunca se muestran, y no se envía nada a ningún sitio al que no se
  le haya dicho que lo envíe.
- **Versiones comprobadas.** Cada versión se revisa con govulncheck antes de
  publicarse, y la última se vuelve a revisar cada día. Las versiones no se pueden
  cambiar una vez publicadas (desde v0.14.0) y llevan procedencia de compilación: `gh attestation
  verify` funciona con cada binario, el chart y la imagen y, a partir de los
  bundles de Sigstore que cada versión guarda desde v0.28.0, también sin conexión
  con los binarios y el chart
  ([Comprobar una versión](https://github.com/lapilli-project/lapilli/blob/main/SECURITY.md#checking-a-release)).
- **A la vista de todos.** Los reportes de seguridad llegan por los avisos de
  seguridad privados de GitHub, y el código lo analizan CodeQL y OpenSSF Scorecard.
- **Para todos los que están de guardia.** Las páginas de `lapilli serve` apuntan a
  WCAG 2.2 AA, con lector de pantalla, solo con el teclado, con un zoom del 200 % y
  en un teléfono, y la CI vigila su estructura y su contraste en todos los idiomas:
  es un objetivo, no una afirmación de que cumplen la norma
  ([Accesibilidad](https://github.com/lapilli-project/lapilli/blob/main/ACCESSIBILITY.md)).

## Ayuda a construirlo {#help-build-it}

Lapilli tiene un mantenedor y busca más, de más de una organización. Hay buenas
preguntas abiertas. ¿Con qué fidelidad puede un backtest reproducir lo que se
disparó? ¿Cómo se llega a un veredicto justo con pocos incidentes anotados?
Varias áreas necesitan lo que el mantenedor actual no tiene: una cuenta de AWS o
Azure con Prometheus gestionado, alertas de Loki en uso real, una herramienta de
gestión de incidentes.

- [Hoja de ruta](https://github.com/lapilli-project/lapilli/blob/main/ROADMAP.md):
  lo que aún no hace, área por área, y dónde vive cada cosa en el código
- [Primeras issues](https://github.com/lapilli-project/lapilli/labels/good%20first%20issue):
  cambios pequeños, cada uno con dónde va y cuándo está terminado; y
  [se busca ayuda](https://github.com/lapilli-project/lapilli/labels/help%20wanted),
  para lo que necesita una cuenta o una herramienta
- [Mantenedores](https://github.com/lapilli-project/lapilli/blob/main/MAINTAINERS.md):
  cómo contribuir en un área lleva a mantenerla
- [Gobernanza](https://github.com/lapilli-project/lapilli/blob/main/GOVERNANCE.md):
  los principios, y cómo se toman las decisiones
- [Cómo contribuir](https://github.com/lapilli-project/lapilli/blob/main/CONTRIBUTING.md):
  compilar, probar, enviar un cambio
- [Discusiones](https://github.com/lapilli-project/lapilli/discussions):
  preguntas, ideas, y lo que Lapilli encontró en tus alertas

El reporte más útil que hay: una regla cuyo backtest difiere de cómo se disparó
realmente.

## Instalar {#install}

<div class="ways">
<div markdown="1">
### Go

```sh
go install lapilli.dev/cmd/lapilli@latest
lapilli demo
```
</div>
<div markdown="1">
### Un binario

Para Linux y macOS, en amd64 o arm64, desde las
[versiones]({{ site.repo }}/releases{% if site.version %}/tag/{{ site.version }}{% endif %}):

```sh
os=$(uname -s | tr '[:upper:]' '[:lower:]'); arch=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
{% if site.version %}curl -fsSL {{ site.repo }}/releases/download/{{ site.version }}/lapilli-{{ site.version }}-$os-$arch.tar.gz | tar -xz
./lapilli-{{ site.version }}-$os-$arch/lapilli demo{% else %}gh release download -R lapilli-project/lapilli -p "lapilli-*-$os-$arch.tar.gz"{% endif %}
```
</div>
<div markdown="1">
### La imagen

```sh
docker run --rm ghcr.io/lapilli-project/lapilli{% if site.version %}:{{ site.version }}{% endif %} demo
```
</div>
<div markdown="1">
### El chart de Helm

Una vez por semana como CronJob, o con `mode=serve` cada hora:

```sh
helm install lapilli oci://ghcr.io/lapilli-project/charts/lapilli -n monitoring \
  --set mode=serve --set prometheus.url=http://prometheus:9090 \
  --set 'rules.configMaps={alert-rules}' --set 'match={service}'
```
</div>
</div>

Cada versión se comprueba antes de publicarse y no se puede cambiar después;
[Comprobar una versión]({{ site.repo }}/blob/main/SECURITY.md#checking-a-release)
te permite verificar lo que descargaste.
[Primeros pasos]({{ '/docs/getting-started/' | relative_url }}) te lleva de
aquí a un primer informe sobre tus propias alertas.

## El nombre {#the-name}

> *mos erat antiquus niveis atrisque lapillis,*<br>
> *his damnare reos, illis absolvere culpa*
>
> Era costumbre antigua juzgar con guijarros blancos y negros: los negros para
> condenar a los acusados, los blancos para absolverlos.
>
> — Ovidio, *Metamorfosis* XV.41–42

Los *lapilli* son piedras pequeñas. Cada disparo es un voto; una vez por semana se
vacía la urna y se cuentan las piedras.
