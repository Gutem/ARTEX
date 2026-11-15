# Endurecimento da implantação

O ARTEX executa shell e tráfego de rede por design. Toda a postura de segurança da
implantação vem de **onde ele roda e de quem alcança o console**, não de o binário
ser inofensivo.

Este documento descreve o que o `docker-compose.override.yml` aplica, por quê, e
como verificar cada item. O arquivo do upstream (`docker-compose.yml`) fica
intocado: o Compose carrega o override sozinho, então o merge com o upstream
continua barato.

## Contexto: a imagem pré-compilada foi comprometida

Em novembro de 2026 a imagem `tomicdd/artex` (padrão do upstream original) foi
encontrada com **backdoor de execução de comandos não autenticada**: o campo
`captcha` do `POST /api/auth/login` executava shell.

Indicadores: `verifyCaptcha`, `tomicdd`, `congdonglinux`.

O caminho oficial deste fork **nunca** usou essa imagem — `.env.example` aponta
para `ruoji6/artex`, construída pelo GitHub Actions do próprio repositório. Os
três indicadores estão ausentes no source e na imagem (`absent` nos três,
verificado com `grep` no binário dentro do container).

## O que o override aplica

| Controle | Antes | Depois | Por quê |
| --- | --- | --- | --- |
| Imagem | `${ARTEX_TAG:-latest}` (flutuante) | `ruoji6/artex@sha256:b4848e32…` | tag flutuante é supply chain sem controle; o vetor acima foi exatamente uma imagem |
| Console | `8787:8787` (todas as interfaces) | `127.0.0.1:8787:8787` | console é RCE para quem alcança a porta |
| Proxy de gravação | `127.0.0.1:8788:8788` | **não publicado** | ele faz MITM do tráfego de teste; expor vaza credencial de cliente |
| Escalada de privilégio | default | `no-new-privileges:true` | o container roda como root de propósito (nmap raw); setuid viraria escada |
| Sistema de arquivos | gravável | `read_only: true` + `tmpfs /tmp` | impede troca do binário e escrita fora dos pontos previstos |
| Chave JWT | `/app/jwt.key` (efêmera) | volume `artex_key` em `/app/keys` | sem isso todo `down`/`up` invalida as sessões |
| Self-update | apontando para o upstream | **desabilitado** | um clique trocaria o binário pelo release oficial e perderia a tradução pt-BR |

### Por que o self-update fica bloqueado

`POST /api/update/apply` recusa quando a versão corrente não é uma release
formal (`selfupdate.CompareVersions` devolve `comparable=false`). A build local
carrega `0.3.30-20-g…-dirty`, então:

```json
{"error":"当前版本 \"0.3.30-20-g40c23c8-dirty\" 不是正式发布版本，已禁用一键更新"}
```

É o mesmo mecanismo que o `failsafe-security/artex` usa para desligar o botão.
Como o `read_only` também impede escrever `artex.new` ao lado do binário, há duas
barreiras independentes.

### `ARTEX_KEY_DIR`

Variável nova, lida por `config.KeyDir()` (`config/config.go`):

- ausente → `config.BaseDir()` (comportamento do upstream, ao lado do executável)
- definida → caminho informado

Sem ela, o container gravaria a chave em `/app`, que com `read_only` recusaria a
escrita e o processo morreria no boot (`log.Fatalf` em `server.New`).

## Verificação

```bash
# 1. a chave nasce no volume e não em /app
docker compose exec -T artex ls /app/keys           # jwt.key
docker compose exec -T artex ls /app/jwt.key        # ausente

# 2. a chave sobrevive a down/up
K1=$(docker compose exec -T artex sha256sum /app/keys/jwt.key | cut -d' ' -f1)
docker compose down && docker compose up -d
K2=$(docker compose exec -T artex sha256sum /app/keys/jwt.key | cut -d' ' -f1)
[ "$K1" = "$K2" ] && echo persistiu

# 3. o proxy não responde do host
curl -m 3 http://127.0.0.1:8788/                 # connection refused

# 4. o container está endurecido
docker inspect artex-artex-1 \
  --format '{{.HostConfig.SecurityOpt}} ro={{.HostConfig.ReadonlyRootfs}}'
# [no-new-privileges:true] ro=true

# 5. o update está bloqueado
curl -X POST -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8787/api/update/apply
# "não é versão oficial, one-click update desabilitado"

# 6. a capacidade foi preservada (nmap raw e egress)
docker compose exec -T artex nmap --version
```

Medições desta implantação (2026-11-14): health `{"ok":true}`; hash da chave
igual antes e depois do `down`/`up`; `8788` recusado; `no-new-privileges:true` e
`ReadonlyRootfs=true`; update recusado com a mensagem acima; nmap 7.93
operacional.

## Build local

O override usa `ARTEX_REF`; o default é o digest do Registry. Para rodar uma
build própria:

```bash
./build.sh --target linux/amd64
mkdir -p dist/amd64 && cp dist/artex-linux-amd64/artex dist/amd64/artex
docker build -t artex:local .
ARTEX_REF=artex:local docker compose up -d
```

## Riscos residuais

1. **Root e Chromium.** O container roda como root (paridade com o upstream, para
   nmap raw e instalação de ferramentas em runtime). O sandbox do Chromium não é
   fronteira aqui.
2. **Execução por design.** Ferramentas Python e servidores MCP stdio executam
   código arbitrário; são configurados por admin. Sessão de admin roubada equivale
   a execução de código.
3. **Proveniência do upstream.** `RuoJi6/ARTEX` é espelho; a build prova que o
   binário corresponde a esta árvore, não que a árvore é idêntica à do autor
   original. Re-diff a cada sincronização.
4. **Dados sensíveis.** A evidência de scan pode conter credencial e dado de
   cliente. O volume `pgdata` e `data/` são sensíveis.
