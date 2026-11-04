# Intelbras Allo para Home Assistant

Integração independente e não oficial para atender o interfone Intelbras Allo Box pelo Home Assistant. Este projeto não é afiliado à Intelbras nem endossado por ela.

**Em desenvolvimento:** este repositório ainda não contém uma integração instalável. As provas de protocolo anteriores não validam a instalação ou o atendimento pelo Home Assistant. As instruções de instalação serão publicadas após a validação da versão completa.

## Escopo da primeira versão

As funções abaixo são objetivos da primeira versão, ainda não disponíveis neste repositório:

- **Instalação e conta:** instalação pelo HACS no Home Assistant Core/Container, com e-mail, senha e seleção de um dispositivo da própria conta. Sem Supervisor, compilação local, serviço administrado à parte, extração de APK, perfil privado do aplicativo ou chave TLS fornecida pelo usuário.
- **Campainha:** evento de acionamento físico filtrado para o dispositivo selecionado, sem gerar um acionamento ao iniciar. Cada instalação terá sua própria identidade de receptor, preservando as notificações do celular.
- **Vídeo e escuta:** vídeo ao vivo e áudio do microfone do interfone.
- **Conversa:** voz do microfone atual do navegador ou iPhone para o interfone enquanto a visualização estiver aberta. A transmissão deverá parar ao soltar o controle, sair da visualização, perder a sessão ou descarregar a integração. Falhas de conexão e autenticação deverão ficar visíveis.

O primeiro alvo é o Allo Box/IDS7969AW em Linux amd64. Outros modelos e plataformas dependerão de validação própria. Não há garantia de áudio simultâneo nos dois sentidos, entrega de exatamente um evento por acionamento ou acesso remoto em qualquer rede.

## Arquitetura e validação pendente

O núcleo de protocolo será um processo Go, com um adaptador Python fino para o Home Assistant. Antes de disponibilizar uma versão, ainda é necessário demonstrar a configuração em uma instalação limpa com apenas a conta do proprietário, o empacotamento pelo HACS e o vídeo e áudio ao vivo, incluindo a conversa pelo microfone do iPhone.

A validação deve preservar a cadeia TLS, os pins exatos aprovados e a lista permitida de destinos, sem redirecionamentos. Chaves privadas, certificados capturados, credenciais e perfis privados não serão distribuídos.

## Fora do escopo

Abertura de portas ou relés, provisionamento por QR, alterações de firmware ou configurações do aparelho, gravação e consulta de histórico, compartilhamento de conta e automações de notificações ou luzes. Uma ponte MQTT independente também não é o produto deste repositório.

Código e acompanhamento: [FidelisClayton/ha-intelbras-allo](https://github.com/FidelisClayton/ha-intelbras-allo). Licença [MIT](LICENSE).
