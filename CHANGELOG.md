# Changelog

Todas as mudanças relevantes deste fork. Formato baseado em
[Keep a Changelog](https://keepachangelog.com/pt-BR/1.1.0/); o projeto segue
versionamento semântico do upstream (`v0.3.30` + commits deste fork).

Este fork parte de `RuoJi6/ARTEX` (espelho de `Autumn-27/ARTEX`, AGPL-3.0) e
mantém três compromissos que orientam tudo abaixo:

1. **Idioma do produto**: interface em pt-BR, comportamento do agente em inglês.
2. **Merge barato com o upstream**: preferir override e camada a reescrever.
3. **O executor não decide escopo**: autorização e intensidade pertencem ao
   Sentinela; o ARTEX executa e registra.

---

## [Não lançado]

### Adicionado

**Canal out-of-band (OAST)** — `internal/oast/` + ferramentas `oast_new_url` e
`oast_hits`.
Resolve a classe de vulnerabilidade que não responde: SSRF cego, XXE cego,
injeção de comando cega, Log4j. O único critério possível é o alvo voltar a um
endereço nosso. Listener HTTP e DNS (`miekg/dns`, já no `go.mod`), token único por
probe com URL e hostname próprios, hit com origem e horário. As ferramentas só
aparecem quando o canal está ligado — quem não usa não paga token no prompt.
Origem: `gthubtom1/ARTEX`.

**HTTP nativo com sessão** — `http_request`, `session_login`.
Todo probe via Bash custava dois round-trips e não tinha sessão: a cadeia
`login → IDOR → releitura` dependia de o modelo carregar cookie na mão. Agora a
requisição roda no processo, pelo mesmo proxy de registro, com cookie jar por
sessão nomeada. Origem: `MythHack/ARTEX`.

**Diff estrutural de respostas** — `http_diff`.
Autorização, lógica de negócio e mass assignment são a mesma operação: comparar
duas respostas. Lado a lado é o que o modelo faz pior. A ferramenta devolve só a
diferença (status, bytes, headers, primeiras linhas divergentes) e diz
explicitamente quando os corpos são **idênticos** — para o agente não inventar
diferença onde não há. Origem: `MythHack/ARTEX`.

**Atualização de prompt embutido por hash** — `db.UpgradeDefaultPromptHash` +
`agent.PromptUpgrades`.
`SeedPromptIfEmpty` é first-insert only: instalação que já tinha versão de prompt
nunca recebia default novo, e a tradução só valeria para banco novo. Reescrever
sempre descartaria a edição do operador, o que é pior. O corte é o hash — quem
nunca editou recebe o texto novo, quem editou continua com o dele. Origem:
`javeil/ARTEX`.

**Decodificação de payload para o juiz** — `intercept.DecodePayload`.
O juiz lia o payload como o modelo escreveu. Base64, percent-encoding, `\uXXXX`,
entidade HTML e hex leem como ruído para o revisor, enquanto o shell decodifica e
executa — assimetria que anula a camada de interceptação, e que o agente produz
sozinho quando um literal quebra o parser. Até 3 níveis aninhados. Origem:
`y1shiny1shin/ARTEX`.

**Gate de conformidade na conclusão** — `db.CompletionBlockedByHardRequirement`.
A interceptação julga uma chamada por vez; ninguém olhava o conjunto antes de a
tarefa se declarar concluída. O gate lê as constraints `deny` da tarefa, extrai
termos verificáveis (descartando genéricos) e recusa a conclusão quando um termo
aparece numa ação registrada, nomeando constraint e ação. Override existe e deixa
trilha. **Determinístico por escolha** — um LLM no caminho de conclusão torna o
resultado irreproduzível. Limite declarado no arquivo: constraint em prosa sem
termo checável nunca casa. Origem: `y1shiny1shin/ARTEX`.

### Alterado

**Prompts de sistema traduzidos para inglês** — 18 blocos.
Conduta (`opsecPromptBlock`, `workerDefaultTmpl`, `pentestDefaultTmpl`),
orquestração (goals, planner, main agent), papéis auxiliares (Auto, asset mapper,
assistant, reporter, retester, credential processor), contexto interno
(compressão, auto-título) e encerramento (3 wrap-ups, teste de conexão).
Identificadores de ferramenta, variáveis `{{.Goal}}`/`{{.Dir}}`, marcadores de
seção e a concatenação de crases do reporter preservados.

**Descrições de ferramentas traduzidas para inglês** — 248 strings em
`agent/tools.go` e `agent/tools_insert.go`.
Eram o maior bloco restante e vão inteiras ao prompt a cada chamada; sem isso o
modelo lia conduta em inglês e contrato em chinês.

**Motivo de cancelamento e de encerramento traduzidos** — 79 strings em
`cancelcause.go` e `terminalreason.go`.
São os blocos que o modelo recebe **depois** de agir, no turno seguinte. Os
textos curtos foram reescritos para caber no limite de 40 runes que o teste já
exigia.

**Compactação preserva por que uma direção parou** — `nodeStopReason` +
regra 3b do prompt.
O digest comprimia a intent sem o motivo de parada e virava "isto foi olhado".
`blocked` (não repetir sem mecanismo novo), `exhausted` (pode continuar) e
`stopped` (matada de propósito) levam a próximos passos opostos — perder a
distinção é como o planejador reabre rota morta ou abandona direção pela metade.
Origem: `hello121384/ARTEX`.

### Segurança

**`ARTEX_KEY_DIR`** — a chave de assinatura JWT morava ao lado do executável e,
no container, fora de qualquer volume: todo `compose up` depois de um `down`
invalidava as sessões. Agora `config.KeyDir()` lê a variável e cai no
comportamento do upstream quando ausente.

**`docker-compose.override.yml`** — endurecimento aplicado sem editar o arquivo
do upstream (o Compose carrega o override sozinho):

| Controle | Antes | Depois |
| --- | --- | --- |
| Imagem | `ruoji6/artex:latest` (flutuante) | `@sha256:b4848e32…` |
| Console | `8787:8787` (todas as interfaces) | `127.0.0.1:8787:8787` |
| Proxy de gravação | publicado | fechado |
| Escalada | default | `no-new-privileges:true` |
| Rootfs | gravável | `read_only: true` + `tmpfs /tmp` |
| Chave JWT | `/app/jwt.key` (efêmera) | volume `artex_key` |
| Self-update | apontando para o upstream | desabilitado em duas camadas |

**Contexto do incidente**: em novembro de 2026 a imagem prebuilt `tomicdd/artex`
foi encontrada com backdoor de execução de comandos não autenticada (campo
`captcha` do login; indicadores `verifyCaptcha`, `tomicdd`, `congdonglinux`).
Nosso caminho nunca usou essa imagem — `.env.example` aponta para
`ruoji6/artex` — e os três indicadores estão ausentes no source e na imagem
(`absent` nos três). Ver `HARDENING.md`.

### Testes

- `agent/prompt_english_test.go` reprova qualquer prompt de sistema que volte a
  ter CJK. Sem isso, o próximo sync do upstream reintroduz chinês em silêncio.
- Suíte do repo: **1.011 funções de teste** passando nos pacotes verdes.
- Falhas pré-existentes em `server/` (43) e `db/` (5) exigem Postgres/`ARTEX_PG_DSN`
  configurado; verificadas idênticas antes e depois das mudanças via `git stash`.

---

## Trabalho fora deste repositório (mesma frente)

Registrado aqui porque é o contexto operacional do fork, não deste código.

### cve-pipeline

- **Downloader do CVEList v5** validava nada: 632 MB chegavam corrompidos e o
  erro só aparecia no `unzip`. Agora confere `Content-Length`, assinatura
  `PK\x03\x04` e `End of Central Directory` **em memória**, rejeita HTML salvo
  como `.zip` e apaga o parcial antes do retry. Timeout 300s → 1800s.
- **Gate de 100 mil CVEs** no workflow: um feed parcial passava por bom e o job
  terminava verde com 30 CVEs. Agora reprova.

### Workflows periódicos (Gitea Actions)

- `feed` (cve-pipeline, diário), `verify` (autokev, semanal), `orchestrate`
  (repo-automation, diário).
- Armadilhas pagas, documentadas em `repo-automation/docs/PIPELINES.md`:
  `secrets` não pode ser avaliado em `if:`; Bun não lê `.npmrc` para registry
  escopado (precisa de `bunfig.toml`); `upload-artifact@v4` falha no Gitea
  (`@v3`); `unzip -v | head -1` mata o step com SIGPIPE; `schedule` só agenda na
  branch padrão; run órfão com `concurrency` trava a fila.

### autokev

- Validado no Gitea de ponta a ponta com Docker e IA: Collector → Builder →
  Exploiter → Judge em 3min44s, veredito `EXPLOIT_FAILED` reprodutível.
- Credenciais do modelo passaram a descer por `env` do job; o artifact passou a
  publicar o log e o exploit gerado (antes subia vazio).

### Referência de forks

Levantamento feito para decidir o que valia trazer: dos **1.185 forks** de
`RuoJi6/ARTEX`, apenas **46** têm commits próprios, e só três fizeram trabalho de
idioma — nós (prompts/tools para inglês), `felipemlopes` (dicionário pt-BR de UI,
3.213 entradas) e `nyanfox87` (OpenCC zh-Hant, conversão de script).

---

## Pendências conhecidas

- **117 strings em chinês** em `agent/` são telemetria e UI de atividade
  (`planner.go` 24, `provider.go` 15, `worker.go` 14, entre outros). Não vão ao
  contrato de ferramenta nem à conduta do agente.
- **Comentários de código** seguem em chinês nos arquivos herdados do upstream.
- **Respostas do agente em inglês**: o prompt define o idioma da saída. Para ele
  responder em português, é uma linha em cada prompt de papel.
- **OAST exige endereço público**: `PublicBase` deve ser o endereço que o **alvo**
  alcança; sem ele o token aponta para o listen local, o que só serve a teste.
- **Gate de conformidade é literal**: constraint em prosa sem termo checável não
  é verificada.
