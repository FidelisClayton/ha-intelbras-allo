# Evidências de configuração inicial

Este documento registra provas de protocolo para o Allo Box/IDS7969AW. O repositório ainda não contém uma integração instalável. Os testes recentes usaram somente o diagnóstico; a instalação de produção não foi alterada.

O objetivo continua sendo configurar a integração com e-mail, senha e seleção de um dispositivo da própria conta, preservando o escopo completo: campainha física, vídeo ao vivo, áudio do microfone do interfone e conversa pelo microfone atual do navegador ou iPhone.

## Resultados verificados

| Teste | Resultado | Limite da evidência |
| --- | --- | --- |
| Nova autenticação nativa, listagem e obtenção de token do dispositivo, sem certificado ou chave TLS de cliente | As três operações passaram com a CA do fornecedor já aprovada, validação de cadeia e pin exato da folha, sem redirecionamentos. | A distribuição pública somente da CA foi aprovada. Identificadores de conta e dispositivo foram conferidos apenas em ambiente privado. |
| Recepção por 6 segundos com `mediaKey` opaca nativa vazia | `PLAY` aceito; vídeo e áudio recebidos. Verificação independente com `ffprobe`: H.264, 480 × 240; PCM `s16le`, 8.000 Hz, mono. | Teste de recepção neste modelo; não valida conversa. |
| Recepção por 6 segundos com chave opaca nativa e `OEM` vazios | `PLAY` aceito; vídeo e áudio recebidos, com a mesma decodificação independente. | Mostra que esses dois valores capturados não foram necessários para a recepção testada. |
| Omissão conjunta de `USERNAME`, `OEM` e `KEY` | Operações de nuvem e preparação LAN passaram; `PLAY` não foi aceito e nenhum quadro foi recebido. | Não isola qual campo causou a falha. O teste posterior com o identificador público fixo do aplicativo recebeu mídia. |
| Recepção por 6 segundos com `USERNAME` público do `SDKConst`, `KEY` e `OEM` vazios | `PLAY` aceito; vídeo H.264, 480 × 240, e áudio PCM `s16le`, 8.000 Hz, mono, verificados por decodificação independente. | Não usa nome de usuário do perfil privado nem chave opaca capturada; não valida conversa. |
| Nova identidade de receptor Google e registro completo de push Intelbras, sem par TLS de cliente | Prova anterior passou. A primeira tentativa Google falhou na etapa `sender`; uma retomada explícita funcionou. | A causa da falha inicial é desconhecida. Não houve novo acionamento físico da campainha para essa identidade. |
| Ponte Home Assistant de produção | Acionamento físico verificado anteriormente. | Não comprova uma nova instalação limpa. |

Os testes LAN usaram um IP explícito já conhecido e conferiram a correspondência com o identificador salvo do dispositivo. Ainda não há prova de descoberta LAN nova. O `SDKConst` do APK declara um nome de usuário fixo do aplicativo, que é um identificador público; seu uso em substituição ao valor retido do perfil privado passou no teste de recepção descrito acima. Nenhum valor desse perfil ou de sua chave é reproduzido aqui.

## Confiança aprovada

O servidor de conta envia somente o certificado folha. Não envia a CA do fornecedor na cadeia e não fornece URL AIA para baixá-la. A CA pública aprovada agora acompanha a biblioteca de configuração inicial.

Foi aprovada uma exceção restrita à regra de não distribuir certificados capturados: incluir somente o certificado público desta CA do fornecedor em `internal/bootstrap/assets/qualvision-ca.pem`:

| Campo | Valor |
| --- | --- |
| Subject e issuer | `O=QUALVISION TECHNOLOGY CO.\,LTD, C=CN, ST=ShangHai` |
| SHA-256 | `cbc77b01d58308cc0fa8ee4af2e508af2cb3864e5a88628b0e7d0e474f4d7763` |
| Validade inicial | 2017-12-11 17:02:16 UTC |
| Validade final | 2037-12-06 17:02:16 UTC |
| Basic Constraints | `CA:TRUE` |

O certificado raiz é público e não envolve chave privada. A exceção não inclui certificado ou chave de cliente, tokens de conta, segredos nativos, APKs ou capturas. Validação da cadeia, pin exato da folha aprovada, lista permitida de destinos e rejeição de redirecionamentos continuam obrigatórios. A exceção restrita foi registrada em `AGENTS.md` e `.gitignore`. O pin SHA-256 DER da folha de conta é `e7167be07fc554614ce611b1be054c29cfc39926695d6a6717c9e631dc83276a`. Como a folha não tem nomes DNS, a biblioteca valida manualmente a cadeia para uso de servidor, o destino fixo `intelbras-4.qvcloud.net:443` e o pin exato. Não há modo inseguro configurável.

## Validações ainda necessárias

- **Configuração com a conta:** a confiança pública e o identificador público do aplicativo estão resolvidos para a recepção testada; falta completar a descoberta LAN e o fluxo de instalação. Os resultados atuais não comprovam configuração completa somente com e-mail e senha.
- **Conversa:** testar o envio sem chave capturada e validar voz do microfone atual do navegador e iPhone, incluindo a interrupção da transmissão nas condições descritas no escopo da primeira versão.
- **Campainha e descoberta:** validar um acionamento físico com uma nova identidade de receptor e a descoberta do dispositivo sem depender do IP conhecido.
- **Versão completa:** validar empacotamento HACS e instalação limpa em Home Assistant Core/Container suportado, no alvo Linux amd64, com todas as funções em hardware real.

## Biblioteca de configuração inicial

`internal/bootstrap` implementa login com e-mail e senha bruta, listagem e seleção de dispositivo e nova solicitação de token antes de preparar as credenciais. A senha é sempre convertida em SHA-256 hexadecimal minúsculo, inclusive quando já contém 64 caracteres. O token deve identificar exatamente o dispositivo selecionado. Senha dinâmica e chave de fluxo permanecem opacas em memória; não há leitura de perfil privado, arquivos padrão na pasta pessoal ou par TLS de cliente. O corpo de autenticação de mídia usa o identificador fixo público `adminapp`, a senha dinâmica e `OEM`/`KEY` vazios.

A biblioteca usa somente a rota de conta aprovada, sem repetição automática, com contextos, limites de tempo, respostas de até 2 MiB e rejeição de redirecionamentos. Os testes offline exercitam HTTP/TLS e XML com certificados e dados sintéticos. Eles não substituem evidências de hardware.

Verificação local do módulo: `go test ./internal/bootstrap` e `go vet ./internal/bootstrap`. A revisão integrada executa também `go test -race ./...` e `go build ./...`. Ainda não há CLI de instalação completa, descoberta LAN validada, implementação pública de registro Google/push ou integração instalável.
