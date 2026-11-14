# Internacionalização: progresso da integração

Usa i18next + react-i18next, preservando o export estático do Next e as URLs existentes. O serviço Go continua servindo os mesmos arquivos estáticos.

Escopo atualizado (2026-11-10): a pedido do usuário, **saída de IA e prompts não entram na internacionalização**. Segue-se traduzindo interface, textos de sistema que não são de IA, descrições embutidas, textos fixos de exportação e notificações; o corpo das saídas do modelo e os prompts não são alterados.

## Idiomas suportados

| Idioma | Código | Estado |
| --- | --- | --- |
| Chinês (padrão) | `zh-CN` | catálogo original `[zh, en]` |
| Inglês | `en` | catálogo original |
| Português (Brasil) | `pt-BR` | **camada adicional** (ver abaixo) |

O chinês é o padrão. O HTML estático e a primeira renderização no cliente usam chinês; após a montagem, o app lê `localStorage.artex_locale`. Quem já salvou preferência por outro idioma pode ver chinês por um instante ao atualizar.

## A camada pt-BR

A tradução para português **não reescreve** o catálogo `[zh, en]`. Ela entra como camada, em arquivos próprios, mesclada em `resources["pt-BR"]`:

```ts
// messages.ts
const enTranslation = Object.fromEntries(
  Object.entries(messages).map(([key, pair]) => [key, pair[1]]),
);

export const resources = {
  "zh-CN": { translation: /* pair[0] */ },
  en: { translation: enTranslation },
  // pt-BR herda o inglês e sobrescreve o que já foi traduzido:
  // chave nova do upstream aparece em inglês em vez de sumir.
  "pt-BR": {
    translation: {
      ...enTranslation,
      ...ptBrMessages,
      ...ptBrOverview,
      ...ptBrChat,
      ...ptBrTasks,
      ...ptBrTasksExtra,
      ...ptBrMisc,
      ...ptBrSettings,
      ...ptBrSchedule,
      ...ptBrNotify,
      ...ptBrScreens,
    },
  },
};
```

| Arquivo | Cobertura |
| --- | --- |
| `pt-br.ts` | casca (navegação, login, conta, datas, layout), visão geral, chat, erros de API, termos de uso |
| `pt-br-tasks-extra.ts` | lista de tarefas, templates, regras de ativos, runtime, fases, menções e paginação |
| `pt-br-settings.ts` | configurações do sistema (tráfego, proxies, MCP, busca, workers, Basic Auth, atualizações) |
| `pt-br-schedule.ts` | agendamentos, editor inline e mensagens de runtime |
| `pt-br-notify.ts` | notificações (canais, filtros, templates, SMTP) |
| `pt-br-screens.json` | `screen-catalog` inteiro (chaves com hash `screen.<hex>`) |

**Por que assim**: o upstream (`RuoJi6/ARTEX`) muda o catálogo com frequência. Se a tradução fosse escrita dentro dos pares `[zh, en]`, todo merge conflitaria em centenas de arquivos. Como camada, o merge é barato e uma chave nova aparece em inglês até ser traduzida.

### Regras para manter

1. **Nunca** adicione o português dentro dos arquivos `*-messages.ts` / `terms.ts` / `screen-catalog.json` do upstream. Escreva nos arquivos `pt-br*`.
2. Chave nova no upstream: cai no inglês automaticamente. Traduza quando aparecer.
3. Chave que não existe no catálogo é **órfã** — o teste `i18n.test.cjs` reprova se você inventar uma.
4. Placeholders (`{{v0}}`, `{{count}}`, `{{name}}`) precisam ser preservados exatamente.
5. Plurais usam `_one` / `_other`; traduza os dois.
6. Dados de demonstração (`web/src/lib/mock/data.ts`) continuam em chinês de propósito: é dado, não interface.

## Já integrado

- Login, inicialização, termos de uso, operações de conta, navegação lateral, busca de páginas, controles de tema e layout.
- Seletor de idioma antes do login e no canto superior direito da barra global; memória no navegador, sincronia entre abas e atualização de `lang`/`title`/`description` da página.
- Status genéricos, paginação, controles de data/hora e textos auxiliares de alguns componentes básicos.
- Alias de busca e nomes de exibição das nove categorias de referência `@`. Textos novos usam kind estável como `@[finding#123 nome]`; front e back continuam reconhecendo o formato chinês histórico.
- Interfaces de autenticação e HTTP Basic Auth ganharam `code`/`params`, mantendo o campo `error` antigo; o front preserva a mensagem original como fallback para código desconhecido.
- Categorias de configurações do sistema e busca, versão e atualizações, verificação de acesso, pool de proxies, IP de saída, chaves de runtime e atalhos; a busca de configurações aceita chinês e inglês. Avisos de saída de proxy ganharam código estruturado, compatível com a string antiga.
- Títulos, legendas, rótulos estatísticos, estados vazios e tempo relativo do painel; lista de conversas, criação/exclusão/renomeação/fixação, área de entrada, anexos, rótulos fixos do histórico de execução, perguntas paralelas, painel de todos e localização de aprovações.
- Página de agendamentos: lista, formulário de criação/edição, data/fuso, linha do tempo de execuções, operações em lote e editor de nome/observações; avisos conhecidos do backend e textos históricos salvos são convertidos na exibição.
- Lista de tarefas, criação de tarefa, gestão de categorias, configuração de concorrência, confirmação de arquivar/restaurar/excluir, controles de template de tarefa, cadeia de configuração de LLM e editor de regras de ativos por tarefa. Descrições de IA, objetivos, corpo de template e protocolo de upload permanecem no original.
- Página de notificações: formulário dos seis tipos de canal, configurações globais, resumo de filtros, lista de entregas, status e horário, erros de validação comuns e avisos do modo demonstração; sintaxe de template de webhook e respostas de terceiros ficam no original.
- Plural em inglês nas contagens; ao trocar de idioma, o formulário de configurações não recarrega, os registros de execução expandidos mostram rótulos no novo idioma e comandos/saídas originais ficam intactos.

## Cobertura de páginas (rodada de 2026-11-10)

As 27 entradas de página principal e as 11 abas do detalhe da tarefa (incluindo inteligência) têm os textos fixos de interface integrados; detalhes em [checklist de aceitação por página](./PAGE-AUDIT.md). As quatro visões da lista de achados, detalhe de achado/mesclagem, reteste, evidência e opções de exportação; ativos, tráfego, repetidor, execução de ferramentas, gravação de LLM, sincronização, workspace; editores de Agent/MCP/Skill/ferramenta/LLM, interceptação de comandos e ativos, histórico de aprovações e logs do sistema também foram migrados.

Foram acrescentados cerca de 2.000 recursos pareados em `screen-catalog.json`. O `ui` de `useUITranslation()` acompanha o idioma da renderização; `liveUI` serve para callbacks estáveis de polling/requisição. Exibição de datas, rótulos de aresta do grafo de exploração, texto do grafo de ativos, nomes e descrições padrão dos agentes embutidos também acompanham o idioma. Nomes e descrições de agente editados pelo usuário ficam no original.

Erros fixos de validação conhecidos e o grupo padrão do Repeater são convertidos apenas na borda de exibição; corpo de requisição e protocolo não são traduzidos. Outros eventos de sistema, erros de negócio, títulos/colunas fixos de arquivos exportados e corpo padrão de notificação ainda precisam de cobertura — não se pode declarar o site inteiramente internacionalizado com base nisso.

A ferramenta de navegador retornou duas vezes `Local browser use is disabled in Work settings` (inclusive com navegador embutido). Portanto a aceitação real por clique em todas as páginas, abertura de todos os diálogos, carga de dados, tela estreita e layout com textos longos em inglês não foi concluída. O teste de renderização automático verifica página inicial, abas de tarefa, tabela de achados com dados e o Repeater; não substitui a aceitação no navegador real.

## Convenções para desenvolvimento futuro

- Registre chinês e inglês juntos em `messages.ts` (ou nos módulos em que ele se divide). Nos componentes, use o `t` de `useTranslation()` ou `useLiveTranslation()`, derivando navegação, status e opções dinamicamente, sem cache de tradução em nível de módulo. O `t` mantém identidade de função que muda com o idioma para compatibilidade com o React Compiler; use `liveT` estável apenas em callbacks de ciclo de vida, para que a mudança de idioma não recarregue dados de formulário.
- IDs de objeto, enums, campos de API, chaves de ferramenta, DSL e kinds de referência permanecem estáveis. Nomes de usuário, observações, templates personalizados, relatórios históricos, requisições/respostas e saídas de comando originais ficam no original.
- Use `useDateLocale()` para datas exibidas; datas armazenadas, formato de API e semântica de fuso ficam como estão. Contagens usam `count` com recursos de plural.
- A explicação de aprovação em `intercept/prompt.go` ainda é analisada por frases fixas em chinês; preserve o prompt e o comportamento de análise conforme o protocolo existente e traduza apenas os rótulos fixos da interface. Convenções históricas como `[模型]`, origem de aprovação manual, “novo diálogo” padrão e o corte por “excede” precisam de compatibilidade ao migrar textos de exibição. O histórico de execução já usa tipos estáveis `command`/`output` em vez de comparar `label === "命令"`, para que rótulos em inglês não afetem a extração de comandos.
- O idioma da interface **não** muda o idioma da saída da IA; saída de modelo e prompts estão fora do escopo.

## Verificação

No diretório `web`, execute `npm run test:i18n`, `node --test src/lib/api-*.test.cjs`, `npx tsc --noEmit --incremental false` e `npm run build:static`.

Na raiz do repositório, execute `go test ./server -run 'TestBasicAuth|TestEgress|TestChatMention|TestValidatePassword|TestProductError|TestPasswordValidationCodes' -count=1`.

Ainda é necessário conferir no navegador: troca de idioma no login/configurações, busca de navegação, memória ao atualizar e entre abas, as nove categorias de referência e rascunhos antigos em chinês, recolhimento da barra lateral e layout móvel. A checagem automatizada atual não cobre essas interações reais. O novo `screen-pages.test.cjs` cobre a renderização inicial em inglês de todas as páginas principais, a troca ida e volta entre os idiomas, as abas do detalhe da tarefa, a tabela de achados com dados e o Repeater, a existência dos recursos de texto fixo e a preservação do conteúdo do usuário.

Verificação complementar de 2026-11-10: as páginas de agendamento, lista de tarefas e notificações migradas ganharam checagem de texto fixo e de chaves ausentes, cobrindo janelas/histórico de agendamento, resumo de arquivamento e limpeza de tarefa, campos de notificação e sintaxe de template de webhook. A automação de navegador continua indisponível, então o clique real e a aceitação visual em tela estreita não foram concluídos.

Complemento da demo local: o agendamento permite criar, editar nome/observações, pausar/ativar, excluir e ver a linha do tempo; o agendamento anexado na criação da tarefa também é salvo. O alias da tarefa de demonstração corresponde ao número usado pelo agendamento via `numeric_id`, evitando `NaN` na seleção. O estado da demonstração fica só na memória do runtime atual e não executa tarefas agendadas reais.
