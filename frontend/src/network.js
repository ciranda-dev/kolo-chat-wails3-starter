const strings = {
  en: {
    'network.public': 'Public network. Run by Ciranda, free, no uptime guarantee while Kolo is a beta.',
    'network.own': 'Your own network, from -bootstrap. Only its entry nodes are used.',
    'network.unreachable.public': 'Kolo Chat could not reach the public network: no entry node answered. Check your internet connection and try again.',
    'network.unreachable.own': 'Kolo Chat could not reach your network: no entry node of -bootstrap answered. Check that they run and that the addresses are right.',
    'network.options': 'You can connect in three ways. Public network, the default: run by Ciranda, free, no uptime guarantee while Kolo is a beta. Your own network: start Kolo Chat with -bootstrap host:port. This computer only: run kolo-node with -first and start Kolo Chat with -bootstrap 127.0.0.1:PORT.',
  },
  es: {
    'network.public': 'Red pública. La gestiona Ciranda, es gratuita y no tiene garantía de disponibilidad mientras Kolo esté en beta.',
    'network.own': 'Tu propia red, de -bootstrap. Solo se usan sus nodos de entrada.',
    'network.unreachable.public': 'Kolo Chat no pudo conectar con la red pública: ningún nodo de entrada respondió. Revisa tu conexión a internet e inténtalo de nuevo.',
    'network.unreachable.own': 'Kolo Chat no pudo conectar con tu red: ningún nodo de entrada de -bootstrap respondió. Comprueba que estén en marcha y que las direcciones sean correctas.',
    'network.options': 'Puedes conectarte de tres maneras. Red pública, la predeterminada: la gestiona Ciranda, es gratuita y no tiene garantía de disponibilidad mientras Kolo esté en beta. Tu propia red: inicia Kolo Chat con -bootstrap host:puerto. Solo este equipo: ejecuta kolo-node con -first e inicia Kolo Chat con -bootstrap 127.0.0.1:PUERTO.',
  },
  pt: {
    'network.public': 'Rede pública. Mantida pela Ciranda, gratuita, sem garantia de disponibilidade enquanto o Kolo estiver em beta.',
    'network.own': 'Sua própria rede, de -bootstrap. Só os nós de entrada dela são usados.',
    'network.unreachable.public': 'O Kolo Chat não conseguiu alcançar a rede pública: nenhum nó de entrada respondeu. Verifique sua conexão com a internet e tente de novo.',
    'network.unreachable.own': 'O Kolo Chat não conseguiu alcançar sua rede: nenhum nó de entrada de -bootstrap respondeu. Verifique se eles estão rodando e se os endereços estão certos.',
    'network.options': 'Você pode se conectar de três formas. Rede pública, a padrão: mantida pela Ciranda, gratuita, sem garantia de disponibilidade enquanto o Kolo estiver em beta. Sua própria rede: inicie o Kolo Chat com -bootstrap host:porta. Só este computador: rode kolo-node com -first e inicie o Kolo Chat com -bootstrap 127.0.0.1:PORTA.',
  },
}

/** The first of en, es or pt that the browser asks for, else en. */
export const networkLanguage = (languages = typeof navigator === 'undefined' ? [] : (navigator.languages ?? [navigator.language])) =>
  languages.map((l) => String(l).slice(0, 2).toLowerCase()).find((l) => l in strings) ?? 'en'

/** The sentence for key in lang. */
export const networkText = (key, lang = networkLanguage()) => (strings[lang] ?? strings.en)[key] ?? strings.en[key] ?? key

/** The payload of AppInfo.network with every field in place: mode is "own" or else "public". */
export const networkInfo = (data) => ({
  mode: data?.mode === 'own' ? 'own' : 'public',
  nodes: Array.isArray(data?.nodes) ? data.nodes.map(String) : [],
})

/** The sentence that says which network the app joins. */
export const networkLine = (info, lang) => networkText(info.mode === 'own' ? 'network.own' : 'network.public', lang)

/** The sentence for a conversation that did not start because no entry node answered, '' for any other failure. */
export const networkFault = (row, lang) => (row?.network === 'own' || row?.network === 'public' ? networkText(`network.unreachable.${row.network}`, lang) : '')
