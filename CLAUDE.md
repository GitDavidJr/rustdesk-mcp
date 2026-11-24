# rustdesk-mcp

Servidor MCP em Go que fala o protocolo do RustDesk. Repositório público: nada de nome de cliente, servidor, ID ou
credencial aqui. `CLAUDE.md` é canônico; `AGENTS.md` aponta para ele.

- `internal/rd/`: protocolo copiado do `rdcli` (MIT), congelado. Ver `terceiros/rdcli/ORIGEM.md`.
- `internal/sessao/`: terminal (comando e saída viajam em base64 entre marcas, sem eco nem prompt), tela (pedido nativo de
  print; o vídeo é descartado) e o gerente das máquinas conectadas.
- `internal/mcpsrv/`: ferramentas, regras de uso (`Instrucoes`) e `scripts/raio-x.ps1` (só leitura).
- `internal/maquinas/` (hash, nunca senha) e `internal/auditoria/` (uma linha por ação, sem senha).

Fatos do RustDesk 1.5.3 que o código depende:
- Terminal, tela, arquivos e túnel são conexões separadas. A senha de uso único morre na primeira que entra; as outras
  entram com o mesmo `SessionID` e nome enquanto houver uma viva (30 s).
- Terminal sem credencial do Windows roda como o usuário logado, sem administrador. Com um administrador, como SYSTEM.
- No Windows o fim do shell só é avisado quando chega mais entrada: por isso o "cutucar".
- O pedido de print força um quadro novo, então funciona com a tela parada.

`go test ./...` · `go build -o bin/rdmcp ./cmd/rdmcp`
