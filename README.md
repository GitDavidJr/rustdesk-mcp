# rustdesk-mcp

MCP (Model Context Protocol) server para operar computadores remotos pelo
**protocolo do [RustDesk](https://rustdesk.com)**, com o seu próprio servidor
(hbbs/hbbr). Dá a um agente de IA (Claude Code, Claude Desktop, Cursor, etc.)
**terminal, tela, mouse, teclado, arquivos e HTTP a partir da rede da máquina**.

Não usa o aplicativo do RustDesk nem tela virtual: fala o protocolo direto, então
roda num servidor sem interface. É um binário Go estático (imagem de ~14 MB). Na
máquina remota fica só o RustDesk comum, que decide quem entra pela senha dela.

Projeto não oficial, sem afiliação com o RustDesk ou a Purslane Ltd. Use só em
máquinas cujo dono autorizou o acesso.

> **Estado:** validado em Windows 11 com RustDesk 1.5.3 e servidor próprio: terminal,
> print, mouse e teclado, aviso de administrador (UAC), arquivos, HTTP pela rede da
> máquina, senha de uso único e senha fixa. Linux e macOS como máquina remota ainda
> não foram testados de verdade.

## Para que serve

- **Integração com sistema legado:** achar o banco, a porta, o ODBC, o arquivo de
  configuração e o jeito de integrar, sem ficar clicando (`raio_x`).
- **Manutenção:** ver por que um serviço ou túnel parou na máquina do cliente.
- **Sistema que só tem interface:** ver a tela, clicar, digitar, usar atalhos.
- **Sistema web interno:** chamar a API ou a página pela rede da máquina (`http`).

## Ferramentas (21)

| Categoria | Ferramentas |
|---|---|
| **Conexão** | `conectar`, `desconectar`, `maquinas`, `maquina_salvar`, `maquina_esquecer` |
| **Terminal** | `executar` (saída limpa e código de saída, ~0,3 s por comando), `terminal_escrever` e `terminal_ler` (sqlcmd, isql, sqlplus...) |
| **Administrador** | `executar_admin` (abre o aviso do Windows e mostra o print) e `admin_resultado` |
| **Mapa** | `raio_x`: sistema, serviços de banco e web, portas, conexões, janelas, atalhos, programas, ODBC, arquivos de banco e configuração, tarefas e rede. Só leitura, Windows |
| **Tela** | `tela` (inteira ou recorte em resolução cheia), `clicar`, `arrastar`, `rolar`, `digitar`, `teclas` |
| **Arquivos** | `arquivos`, `arquivo_baixar`, `arquivo_enviar` |
| **Rede** | `http` |

Depois de clicar, rolar ou digitar, o print de como ficou já volta na resposta. As
coordenadas são as da última imagem inteira: a conversão para o monitor é automática.

## Pré-requisitos

- Um servidor RustDesk seu (hbbs/hbbr) e a chave pública dele (`id_ed25519.pub`).
- Na máquina remota: RustDesk instalado apontando para esse servidor. Terminal pede
  **RustDesk 1.4.1+** e **Windows 10 1809+**; em Windows mais antigo sobra a tela.
- Permissões da instalação padrão: terminal, teclado/mouse, arquivos e túnel TCP.

## Instalação

```bash
go install github.com/GitDavidJr/rustdesk-mcp/cmd/rdmcp@latest
```

Claude Code, rodando na sua máquina:

```bash
claude mcp add rustdesk \
  -e RDMCP_SERVIDOR=rustdesk.exemplo.com \
  -e RDMCP_CHAVE='CHAVE_PUBLICA_DO_SERVIDOR' \
  -- rdmcp stdio
```

Num servidor, por HTTP (veja `docker-compose.yml`):

```bash
claude mcp add --transport http rustdesk https://mcp.exemplo.com/ \
  --header "Authorization: Bearer $RDMCP_TOKEN"
```

## Configuração

| Variável | Para quê |
|---|---|
| `RDMCP_SERVIDOR` | hbbs: `host[:21116]`. Obrigatória |
| `RDMCP_CHAVE` | chave pública do servidor. Obrigatória: é com ela que a identidade da máquina é conferida |
| `RDMCP_RELAY` | hbbr, se não for o que o hbbs anuncia |
| `RDMCP_TOKEN` | modo `http`: token de pelo menos 32 caracteres |
| `RDMCP_OUVIR` | modo `http`: endereço (padrão `127.0.0.1:8090`) |
| `RDMCP_NOME` | nome que a pessoa vê na janela de conexão |
| `RDMCP_REGRAS` | arquivo de texto com regras da sua casa, somadas às instruções do servidor |
| `RDMCP_OCIOSO` | desconecta depois desse tempo parado (padrão `30m`) |
| `RDMCP_DADOS` | cadastro, auditoria e arquivos baixados |

## Segurança

- **Autorização:** `conectar` exige o nome do cliente e quem autorizou. A pessoa vê a
  janela de conexão do RustDesk o tempo todo.
- **Identidade:** a máquina é conferida com a chave do servidor; sem ela, não conecta.
- **Senha:** não é gravada. O acesso fixo guarda o hash salgado do RustDesk, que só
  entra naquela máquina e deixa de valer quando a senha muda. Trate o cadastro como
  segredo mesmo assim.
- **Auditoria:** uma linha JSON por ação em `auditoria.jsonl`, sem senha.
- **Exposição:** o modo `http` dá acesso às suas máquinas. Deixe atrás de TLS e de
  mais uma barreira (VPN, cabeçalho secreto no proxy, Cloudflare Access).

## Como é o acesso no Windows

- O terminal roda como o **usuário logado, sem administrador**, e exige alguém logado.
- Para administrador: `executar_admin` pede a elevação e o aviso do Windows aparece no
  print para confirmar, com o ok do dono. Ou informe um administrador do Windows no
  `conectar`, e o terminal roda como SYSTEM.
- Sem ninguém logado (tela de login), o terminal não abre; a tela abre.
- A senha de uso único vale uma entrada. O servidor usa uma sessão só para terminal,
  tela e arquivos, então uma senha basta enquanto houver uma conexão viva.

## Desenvolvimento

```bash
go test ./...
go build -o bin/rdmcp ./cmd/rdmcp
RDMCP_SENHA=... bin/rdmcp cli 123456789 executar hostname
```

## Créditos e licença

MIT. A camada de protocolo (`internal/rd`) vem do
[rdcli](https://github.com/bropines/rdcli), também MIT: veja `terceiros/rdcli`.
Nenhum código do RustDesk está incluído. RustDesk é marca dos seus donos.
