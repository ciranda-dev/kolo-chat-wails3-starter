import test from 'node:test'
import assert from 'node:assert/strict'
import { networkFault, networkInfo, networkLanguage, networkLine, networkText } from './network.js'

test('networkInfo is the public network unless the mode says own', () => {
  assert.deepEqual(networkInfo(undefined), { mode: 'public', nodes: [] })
  assert.deepEqual(networkInfo({ mode: 'own', nodes: ['127.0.0.1:4000'] }), { mode: 'own', nodes: ['127.0.0.1:4000'] })
  assert.deepEqual(networkInfo({ mode: 'other', nodes: null }), { mode: 'public', nodes: [] })
})

test('networkLine and networkFault name the network in use', () => {
  assert.equal(networkLine(networkInfo({ mode: 'public' }), 'en'), networkText('network.public', 'en'))
  assert.equal(networkLine(networkInfo({ mode: 'own' }), 'en'), networkText('network.own', 'en'))
  assert.equal(networkFault({ network: 'public' }, 'en'), networkText('network.unreachable.public', 'en'))
  assert.equal(networkFault({ network: 'own' }, 'es'), networkText('network.unreachable.own', 'es'))
  assert.equal(networkFault({ network: '' }, 'en'), '')
  assert.equal(networkFault({}, 'en'), '')
})

test('networkLanguage picks en, es or pt', () => {
  assert.equal(networkLanguage(['pt-BR', 'en']), 'pt')
  assert.equal(networkLanguage(['fr', 'es-MX']), 'es')
  assert.equal(networkLanguage(['fr']), 'en')
  assert.equal(networkLanguage([]), 'en')
})

test('every language names the three ways to connect', () => {
  for (const lang of ['en', 'es', 'pt']) {
    assert.match(networkText('network.options', lang), /-bootstrap host:/, lang)
    assert.match(networkText('network.options', lang), /-first/, lang)
    assert.match(networkText('network.public', lang), /Ciranda/, lang)
  }
  for (const lang of ['es', 'pt']) {
    for (const key of ['network.public', 'network.own', 'network.unreachable.public', 'network.unreachable.own', 'network.options']) {
      assert.notEqual(networkText(key, lang), networkText(key, 'en'), `${lang} ${key}`)
    }
  }
})
