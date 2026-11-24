# Camada de protocolo (internal/rd)

Copiada de https://github.com/bropines/rdcli, commit `c3ebfeca` (23/11/2026), licença MIT (ver LICENSE e NOTICE aqui).
Revisada em 24/11/2026: sem execução de processo, sem endereço fixo, rede só por `wire` (net.Dialer e websocket).
Fica congelada aqui: não há dependência do repositório deles. Mudanças nossas: `peer.Options.SessionID` (uma sessão só para terminal, tela e arquivos) e `ScreenshotRequest`/`ScreenshotResponse`
no `proto/session.proto` (campos 29 e 30 do RustDesk 1.5.3) e o `pb` gerado de novo.
Não entrou: a CLI, o login no painel (`api`), o console (`term`) e a configuração deles.
