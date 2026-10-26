#!/usr/bin/env python3
"""Genera los planes JMeter de vibranium-orderbook-meli.

    python3 jmeter/generate_plans.py        # reescribe jmeter/vibranium-load.jmx y jmeter/vibranium-integrity.jmx

Los .jmx son el entregable (se pueden abrir en la GUI de JMeter); este script existe para que el XML no se edite a mano y
para poder revisar la lógica de validación (Groovy) como código normal. Todo se parametriza con propiedades -J (ver
jmeter/README.md): nada sensible vive en el plan (la API no tiene credenciales).
"""
import re
from pathlib import Path
from xml.sax.saxutils import escape

HERE = Path(__file__).resolve().parent


def esc(s: str) -> str:
    return escape(s, {'"': "&quot;"})


# ---------------------------------------------------------------- piezas XML
def tag(name, text="", **attrs):
    a = "".join(f' {k}="{esc(str(v))}"' for k, v in attrs.items())
    return f"<{name}{a}>{esc(text)}</{name}>" if text != "" else f"<{name}{a}/>"


def sprop(name, value):
    return f'<stringProp name="{esc(name)}">{esc(str(value))}</stringProp>'


def bprop(name, value):
    return f'<boolProp name="{esc(name)}">{"true" if value else "false"}</boolProp>'


def iprop(name, value):
    return f'<intProp name="{esc(name)}">{value}</intProp>'


def ht(inner=""):
    return f"<hashTree>{inner}</hashTree>"


def element(cls, gui, testname, body, enabled=True, extra_attrs=""):
    return (f'<{cls} guiclass="{gui}" testclass="{cls}" testname="{esc(testname)}" enabled="{str(enabled).lower()}"{extra_attrs}>'
            f"{body}</{cls}>")


def arguments(pairs, cls_name="Arguments", gui="ArgumentsPanel", testname="User Defined Variables"):
    items = "".join(
        f'<elementProp name="{esc(k)}" elementType="Argument">{sprop("Argument.name", k)}{sprop("Argument.value", v)}'
        f'{sprop("Argument.metadata", "=")}</elementProp>' for k, v in pairs)
    return element(cls_name, gui, testname, f'<collectionProp name="Arguments.arguments">{items}</collectionProp>')


def test_plan(name, comments, variables):
    udv = ('<elementProp name="TestPlan.user_defined_variables" elementType="Arguments" guiclass="ArgumentsPanel" '
           'testclass="Arguments" testname="User Defined Variables" enabled="true">'
           '<collectionProp name="Arguments.arguments">' +
           "".join(f'<elementProp name="{esc(k)}" elementType="Argument">{sprop("Argument.name", k)}'
                   f'{sprop("Argument.value", v)}{sprop("Argument.metadata", "=")}</elementProp>' for k, v in variables) +
           "</collectionProp></elementProp>")
    body = (sprop("TestPlan.comments", comments) + bprop("TestPlan.functional_mode", False) +
            bprop("TestPlan.tearDown_on_shutdown", True) + bprop("TestPlan.serialize_threadgroups", False) + udv +
            sprop("TestPlan.user_define_classpath", ""))
    return element("TestPlan", "TestPlanGui", name, body)


def http_defaults():
    body = (sprop("HTTPSampler.domain", "${__P(host,localhost)}") + sprop("HTTPSampler.port", "${__P(port,8080)}") +
            sprop("HTTPSampler.protocol", "${__P(protocol,http)}") + sprop("HTTPSampler.contentEncoding", "UTF-8") +
            sprop("HTTPSampler.path", "") + sprop("HTTPSampler.implementation", "HttpClient4") +
            sprop("HTTPSampler.connect_timeout", "${__P(connect_timeout_ms,3000)}") +
            sprop("HTTPSampler.response_timeout", "${__P(response_timeout_ms,10000)}") +
            '<elementProp name="HTTPsampler.Arguments" elementType="Arguments" guiclass="HTTPArgumentsPanel" '
            'testclass="Arguments" testname="User Defined Variables" enabled="true"><collectionProp name="Arguments.arguments"/></elementProp>')
    return element("ConfigTestElement", "HttpDefaultsGui", "Servidor bajo prueba (host, port, protocol)", body)


def header_manager():
    h = ('<elementProp name="" elementType="Header">' + sprop("Header.name", "Content-Type") +
         sprop("Header.value", "application/json") + "</elementProp>"
         '<elementProp name="" elementType="Header">' + sprop("Header.name", "Accept") +
         sprop("Header.value", "application/json") + "</elementProp>")
    return element("HeaderManager", "HeaderPanel", "Cabeceras JSON", f'<collectionProp name="HeaderManager.headers">{h}</collectionProp>')


def http_sampler(name, method, path, body=None):
    if body is not None:
        args = ('<elementProp name="HTTPsampler.Arguments" elementType="Arguments" guiclass="HTTPArgumentsPanel" '
                'testclass="Arguments" testname="User Defined Variables" enabled="true"><collectionProp name="Arguments.arguments">'
                '<elementProp name="" elementType="HTTPArgument">' + bprop("HTTPArgument.always_encode", False) +
                sprop("Argument.value", body) + sprop("Argument.metadata", "=") + "</elementProp></collectionProp></elementProp>")
    else:
        args = ('<elementProp name="HTTPsampler.Arguments" elementType="Arguments" guiclass="HTTPArgumentsPanel" '
                'testclass="Arguments" testname="User Defined Variables" enabled="true"><collectionProp name="Arguments.arguments"/></elementProp>')
    b = (bprop("HTTPSampler.postBodyRaw", body is not None) + args + sprop("HTTPSampler.domain", "") + sprop("HTTPSampler.port", "") +
         sprop("HTTPSampler.protocol", "") + sprop("HTTPSampler.path", path) + sprop("HTTPSampler.method", method) +
         bprop("HTTPSampler.follow_redirects", False) + bprop("HTTPSampler.auto_redirects", False) +
         bprop("HTTPSampler.use_keepalive", True) + bprop("HTTPSampler.DO_MULTIPART_POST", False) +
         bprop("HTTPSampler.BROWSER_COMPATIBLE_MULTIPART", False) + sprop("HTTPSampler.embedded_url_re", ""))
    return element("HTTPSamplerProxy", "HttpTestSampleGui", name, b)


def _no_gstrings(script: str) -> str:
    """JMeter sustituye ${...} dentro del texto de los scripts. Para que NINGUNA expresión Groovy sea confundida con una
    variable de JMeter, las interpolaciones "${x}" se convierten en concatenación: " + (x) + "."""
    return re.sub(r"\$\{([^{}]+)\}", lambda m: '" + String.valueOf(' + m.group(1) + ') + "', script)


def jsr223(kind, name, script, cache_key):
    """kind: JSR223Assertion | JSR223PostProcessor | JSR223PreProcessor | JSR223Sampler"""
    gui = {"JSR223Assertion": "TestBeanGUI", "JSR223PostProcessor": "TestBeanGUI", "JSR223PreProcessor": "TestBeanGUI",
           "JSR223Sampler": "TestBeanGUI"}[kind]
    body = (sprop("scriptLanguage", "groovy") + sprop("parameters", "") + sprop("filename", "") + sprop("cacheKey", cache_key) +
            sprop("script", _no_gstrings(script)))
    return element(kind, gui, name, body)


def thread_group(name, threads, ramp, duration, loops="-1", scheduler=True, on_error="continue", delay="0", cls="ThreadGroup"):
    body = (sprop("ThreadGroup.on_sample_error", on_error) +
            '<elementProp name="ThreadGroup.main_controller" elementType="LoopController" guiclass="LoopControlPanel" '
            'testclass="LoopController" testname="Loop Controller" enabled="true">' + bprop("LoopController.continue_forever", False) +
            sprop("LoopController.loops", loops) + "</elementProp>" +
            sprop("ThreadGroup.num_threads", threads) + sprop("ThreadGroup.ramp_time", ramp) + bprop("ThreadGroup.scheduler", scheduler) +
            sprop("ThreadGroup.duration", duration) + sprop("ThreadGroup.delay", delay) + bprop("ThreadGroup.same_user_on_next_iteration", True))
    return element(cls, "ThreadGroupGui" if cls == "ThreadGroup" else "SetupThreadGroupGui" if cls == "SetupThreadGroup" else "PostThreadGroupGui",
                   name, body)


def if_controller(name, cond):
    return element("IfController", "IfControllerPanel", name,
                   sprop("IfController.condition", cond) + bprop("IfController.evaluateAll", False) + bprop("IfController.useExpression", True))


def throughput_controller(name, percent):
    return element("ThroughputController", "ThroughputControllerGui", name,
                   iprop("ThroughputController.style", 1) + bprop("ThroughputController.perThread", False) + iprop("ThroughputController.maxThroughput", 1) +
                   sprop("ThroughputController.percentThroughput", percent))


def const_throughput_timer(name):
    # calcMode 4 = "all active threads in current thread group (shared)"; el valor es en muestras POR MINUTO.
    return element("ConstantThroughputTimer", "TestBeanGUI", name,
                   iprop("calcMode", 4) + sprop("throughput", "${__jexl3(${__P(tps,0)} > 0 ? ${__P(tps,0)} * 60 : 60000000)}"))


def simple_controller(name):
    return element("GenericController", "LogicControllerGui", name, "")


def while_controller(name, cond):
    return element("WhileController", "WhileControllerGui", name, sprop("WhileController.condition", cond))


def test_action_pause(name, ms):
    return element("TestAction", "TestActionGui", name,
                   iprop("ActionProcessor.action", 1) + iprop("ActionProcessor.target", 0) + sprop("ActionProcessor.duration", ms))


def node(open_el, children=""):
    return open_el + ht(children)


# ---------------------------------------------------------------- scripts Groovy (validación de NEGOCIO)
COMMON = r"""
import groovy.json.JsonSlurper
def fail = { String msg -> AssertionResult.setFailure(true); AssertionResult.setFailureMessage(msg) }
def code = prev.getResponseCode()
def text = prev.getResponseDataAsString()
"""

ASSERT_ORDER = COMMON + r"""
// POST /orders válido: debe ser 201 y el cuerpo debe ser coherente consigo mismo y con lo enviado.
if (code != '201') { fail("esperado 201, recibido " + code + " " + (text.length() > 120 ? text.substring(0, 120) : text)); return }
def r = new JsonSlurper().parseText(text)
def o = r.order
if (o == null) { fail("sin 'order' en la respuesta"); return }
def id = vars.get('orderId'); def side = vars.get('side')
long price = vars.get('price') as long; long qty = vars.get('qty') as long
if (o.id != id) { fail("id devuelto ${o.id} != enviado ${id}"); return }
if (o.user_id != vars.get('userId') || o.side != side || o.price != price || o.qty != qty) { fail("la orden devuelta no coincide con la enviada"); return }
if (o.filled < 0 || o.filled > o.qty || o.filled + o.remaining != o.qty) { fail("filled+remaining != qty: ${o.filled}+${o.remaining} vs ${o.qty}"); return }
def trades = r.trades ?: []
long sum = 0
for (t in trades) {
  sum += t.qty
  if (t.taker_order_id != id) { fail("trade con taker ${t.taker_order_id} != ${id}"); return }
  if (t.total != t.price * t.qty) { fail("trade.total != price*qty"); return }
  if (side == 'BUY') {
    if (t.price > price) { fail("compra a ${t.price} > límite ${price}"); return }
    if (t.buy_order_id != id) { fail("trade de compra con buy_order_id ajeno"); return }
  } else {
    if (t.price < price) { fail("venta a ${t.price} < límite ${price}"); return }
    if (t.sell_order_id != id) { fail("trade de venta con sell_order_id ajeno"); return }
  }
}
if (sum != o.filled) { fail("suma de trades ${sum} != filled ${o.filled}"); return }
def okStatus = (o.filled == o.qty) ? 'FILLED' : (o.filled == 0 ? 'OPEN' : 'PARTIALLY_FILLED')
if (o.status != okStatus && o.status != 'CANCELLED') { fail("status ${o.status} incoherente con filled=${o.filled}/${o.qty} (esperado ${okStatus})"); return }
vars.put('lastStatus', o.status)
vars.put('lastId', id)
"""

ASSERT_CANCEL = COMMON + r"""
// DELETE /orders/{id}: 200 con la orden CANCELLED, o 404 order_not_found si otro hilo la ejecutó antes (carrera legítima).
def r = new JsonSlurper().parseText(text)
if (code == '200') {
  if (r.status != 'CANCELLED' || r.id != vars.get('lastId')) fail("cancelación incoherente: ${text}")
} else if (code == '404') {
  prev.setSuccessful(true)   // carrera legítima: otro hilo ya ejecutó/canceló la orden
  if (r.code != 'order_not_found') fail("404 sin code order_not_found: ${text}")
} else fail("esperado 200/404, recibido " + code)
"""

ASSERT_BALANCE = COMMON + r"""
if (code != '200') { fail("esperado 200, recibido " + code); return }
def b = new JsonSlurper().parseText(text)
if (b.user_id != vars.get('userId')) { fail("user_id incorrecto"); return }
if (b.brl_free < 0 || b.brl_held < 0 || b.vib_free < 0 || b.vib_held < 0) fail("saldo negativo: ${text}")
"""

ASSERT_BOOK = COMMON + r"""
if (code != '200') { fail("esperado 200, recibido " + code); return }
def b = new JsonSlurper().parseText(text)
def bids = b.bids ?: []; def asks = b.asks ?: []
for (int i = 1; i < bids.size(); i++) if (bids[i].price >= bids[i-1].price) { fail("bids no están en orden decreciente"); return }
for (int i = 1; i < asks.size(); i++) if (asks[i].price <= asks[i-1].price) { fail("asks no están en orden creciente"); return }
if (bids && asks && bids[0].price >= asks[0].price) fail("LIBRO CRUZADO: mejor compra ${bids[0].price} >= mejor venta ${asks[0].price}")
"""


def expect(status, code_field=None):
    """Aserción para errores ESPERADOS: el código HTTP y el `code` del cuerpo deben ser los del contrato."""
    extra = ""
    if code_field:
        extra = (f"def r = new JsonSlurper().parseText(text)\n"
                 f"if (r.code != '{code_field}') fail(\"code esperado {code_field}, recibido ${{r.code}}\")\n")
    # JMeter marca como fallido todo 4xx/5xx; aquí ese código ES el resultado correcto, así que la aserción (que valida código Y cuerpo)
    # pasa a ser la única que decide: se desmarca el fallo del muestreador y se vuelve a marcar solo si el contrato no se cumple.
    return COMMON + "prev.setSuccessful(true)\n" + f"if (code != '{status}') {{ fail(\"esperado {status}, recibido \" + code + \" \" + text); return }}\n" + extra


HDR_BAND = COMMON + r"""
// Esta prueba es DETERMINISTA: necesita que ninguna orden ajena se cruce con las suyas. Un comprador con límite P1 se cruza con TODA oferta
// <= P1 y un vendedor con límite P0 con TODA demanda >= P0, así que la banda [P0, P1] debe caer en el hueco entre la mejor compra y la
// mejor venta del libro (o en cualquier lugar si el libro está vacío). Si no hay hueco de al menos 3 centavos, falla con la causa explícita.
if (code != '200') { fail("GET /book: esperado 200, recibido " + code); return }
def b = new JsonSlurper().parseText(text)
long bestBid = b.bids ? (b.bids.collect { it.price as long }.max()) : 0L
long bestAsk = b.asks ? (b.asks.collect { it.price as long }.min()) : Long.MAX_VALUE
long lo = bestBid + 1
long hi = (bestAsk == Long.MAX_VALUE) ? Math.max(lo + 1000L, 1000000L) : bestAsk - 1
if (hi - lo < 2) {
  fail("PRECONDICIÓN NO CUMPLIDA: el libro no tiene hueco para aislar esta prueba (mejor compra=" + bestBid + ", mejor venta=" + bestAsk + "). " +
       "Ejecútela con el libro vacío (docker compose down -v && up -d) o cuando haya al menos 3 centavos entre la mejor compra y la mejor venta.")
  return
}
long base = (props.getProperty('base_price') ?: '777000') as long
long p0 = (base >= lo && base + 100 <= hi) ? base : lo + (hi - lo).intdiv(3)
long p1 = Math.min(hi, p0 + 100)
if (p1 <= p0) p1 = p0 + 1
vars.put('P0', p0.toString()); vars.put('P1', p1.toString()); vars.put('BRL0', (20L * p1).toString())
"""

PRE_RUNID = r"""
// Un identificador único por ejecución: evita colisiones de idempotencia entre corridas (los ids de orden se recuerdan
// en una ventana de 5 M) y permite reconocer, y no mezclar, los datos que creó esta prueba.
if (props.getProperty('run_id') == null || props.getProperty('run_id').isEmpty()) {
  props.setProperty('run_id', new Date().format('yyyyMMddHHmmss') + Integer.toString(new Random().nextInt(0x10000), 16).padLeft(4, '0'))
}
"""

PRE_ORDER = r"""
// Genera una orden válida y única: id = jm<run>-<hilo>-<n>. Precio en centavos alrededor del precio medio; así hay cruces y parciales.
def rnd = java.util.concurrent.ThreadLocalRandom.current()
int users = (props.getProperty('users') ?: '100') as int
long mid = (props.getProperty('mid_price') ?: '10000') as long
long spread = (props.getProperty('spread') ?: '50') as long
String run = props.getProperty('run_id')
long n = vars.getIteration()
vars.put('orderId', "jm${run}-${ctx.getThreadNum()}-${n}")
vars.put('userId', "jm${run}-u${rnd.nextInt(users)}")
vars.put('side', rnd.nextBoolean() ? 'BUY' : 'SELL')
vars.put('price', Long.toString(mid + rnd.nextLong(2 * spread + 1) - spread))
vars.put('qty', Long.toString(1 + rnd.nextInt(10)))
vars.put('lastId', "jm${run}-${ctx.getThreadNum()}-${n}"); vars.put('lastStatus', '')
"""

PRE_FUND_USER = r"""
String run = props.getProperty('run_id')
vars.put('fundUser', "jm${run}-u${vars.get('__jm__fund__idx')}")
"""

POST_AUDIT_BASELINE = r"""
// Línea base de la auditoría ANTES de fondear: al final, el total debe ser base + lo depositado (conservación bajo carga).
// Es válido si nadie más deposita durante la prueba (ver README: no usar contra un sistema con otros clientes).
if (prev.getResponseCode() == '200') {
  def a = new groovy.json.JsonSlurper().parseText(prev.getResponseDataAsString())
  props.setProperty('base_brl', a.total_brl.toString()); props.setProperty('base_vib', a.total_vib.toString())
}
"""

ASSERT_AUDIT_FINAL = COMMON + r"""
// Conservación de valor tras toda la carga: el motor no creó ni destruyó dinero ni unidades.
if (code != '200') { fail("auditoría: esperado 200, recibido " + code + " " + text); return }
def a = new JsonSlurper().parseText(text)
if (a.ok != true) { fail("INVARIANTES ROTOS: " + text); return }
int users = (props.getProperty('users') ?: '100') as int
long fundBrl = (props.getProperty('fund_brl') ?: '100000000000') as long
long fundVib = (props.getProperty('fund_vib') ?: '100000000') as long
long baseBrl = (props.getProperty('base_brl') ?: '0') as long
long baseVib = (props.getProperty('base_vib') ?: '0') as long
long expBrl = baseBrl + users * fundBrl; long expVib = baseVib + users * fundVib
if (a.total_brl != expBrl || a.total_vib != expVib)
  fail("conservación rota: BRL ${a.total_brl} (esperado ${expBrl}), VIB ${a.total_vib} (esperado ${expVib})")
"""


# ---------------------------------------------------------------- plan de CARGA
def load_plan():
    unique = "jm${__P(run_id)}"
    order_body = '{"id":"${orderId}","user_id":"${userId}","side":"${side}","price":${price},"qty":${qty}}'

    sampler_order = node(http_sampler("POST /orders", "POST", "/orders", order_body),
                         node(jsr223("JSR223PreProcessor", "Generar orden única", PRE_ORDER, "pre_order"), "") +
                         node(jsr223("JSR223Assertion", "Validar orden: código, coherencia y trades", ASSERT_ORDER, "assert_order"), "") +
                         node(const_throughput_timer("Tasa objetivo de órdenes/s (-Jtps; 0 = sin tope)"), ""))

    # Más simple y barato que mezclar jexl y JavaScript: un Throughput Controller en modo porcentaje.
    cancel = node(throughput_controller("Cancelar (cancel_pct %)", "${__P(cancel_pct,3)}"),
                  node(if_controller("Solo si la orden quedó en el libro", "${__groovy(vars.get('lastStatus') == 'OPEN' || vars.get('lastStatus') == 'PARTIALLY_FILLED')}"),
                       node(http_sampler("DELETE /orders/{id}", "DELETE", "/orders/${lastId}"),
                            node(jsr223("JSR223Assertion", "Validar cancelación", ASSERT_CANCEL, "assert_cancel"), ""))))

    balance = node(throughput_controller("Consultar saldo (4 %)", "4"),
                   node(http_sampler("GET /balances/{user}", "GET", "/balances/${userId}"),
                        node(jsr223("JSR223Assertion", "Validar saldo no negativo", ASSERT_BALANCE, "assert_balance"), "")))
    book = node(throughput_controller("Consultar libro (1 %)", "1"),
                node(http_sampler("GET /book?depth=5", "GET", "/book?depth=5"),
                     node(jsr223("JSR223Assertion", "Validar libro no cruzado y ordenado", ASSERT_BOOK, "assert_book"), "")))

    def neg(label, pct, method, path, body, exp_code, code_field):
        return node(throughput_controller(f"Error esperado: {label} ({pct} %)", pct),
                    node(http_sampler(f"ERR-ESPERADO {label}", method, path, body),
                         node(jsr223("JSR223Assertion", f"Validar {exp_code} {code_field or ''}".strip(), expect(exp_code, code_field), f"exp_{exp_code}_{code_field}"), "")))

    negatives = "".join([
        neg("lado inválido -> 400", "0.1", "POST", "/orders", '{"user_id":"${userId}","side":"HOLD","price":10000,"qty":1}', "400", "invalid_side"),
        neg("id duplicado -> 409", "0.1", "POST", "/orders", '{"id":"${lastId}","user_id":"${userId}","side":"BUY","price":1,"qty":1}', "409", "duplicate_order"),
        neg("sin saldo -> 422", "0.1", "POST", "/orders", '{"user_id":"jm${__P(run_id)}-sin-saldo","side":"BUY","price":10000,"qty":1}', "422", "insufficient_funds"),
        neg("id inválido -> 400", "0.05", "POST", "/orders", '{"id":"id con espacios","user_id":"${userId}","side":"BUY","price":10000,"qty":1}', "400", "invalid_id"),
        neg("cantidad 0 -> 400", "0.05", "POST", "/orders", '{"user_id":"${userId}","side":"BUY","price":10000,"qty":0}', "400", "invalid_request"),
        neg("cancelar inexistente -> 404", "0.05", "DELETE", "/orders/jm${__P(run_id)}-no-existe", None, "404", "order_not_found"),
    ])
    history = node(if_controller("Solo con historial (-Jwith_history=true, requiere Postgres)", "${__groovy(props.getProperty('with_history') == 'true')}"),
                   node(throughput_controller("Consultar historial (0,5 %)", "0.5"),
                        node(http_sampler("GET /orders/{id} (historial)", "GET", "/orders/${lastId}"),
                             node(jsr223("JSR223Assertion", "200 o 404 (aún no proyectada)",
                                         COMMON + "if (code == '404') prev.setSuccessful(true)\nif (code != '200' && code != '404') fail('esperado 200/404, recibido ' + code)\n", "assert_hist"), ""))))

    main_children = (header_manager() + ht() + sampler_order + cancel + balance + book + negatives + history)

    # setUp (UN solo grupo, secuencial): línea base -> fondeo de usuarios. Va antes de la carga; si falla, la corrida no es válida.
    loop_fund = element("LoopController", "LoopControlPanel", "fund", bprop("LoopController.continue_forever", True) +
                        sprop("LoopController.loops", "${__P(users,100)}"))
    setup = node(thread_group("Preparación (línea base y fondeo)", "1", "0", "0", loops="1", scheduler=False, cls="SetupThreadGroup"),
                 header_manager() + ht() +
                 node(http_sampler("SETUP GET /audit (línea base)", "GET", "/audit"),
                      node(jsr223("JSR223PreProcessor", "Fijar run_id único", PRE_RUNID, "pre_runid"), "") +
                      node(jsr223("JSR223PostProcessor", "Guardar totales base", POST_AUDIT_BASELINE, "post_base"), "")) +
                 node(loop_fund,
                      node(http_sampler("SETUP POST /deposits", "POST", "/deposits",
                                        '{"user_id":"${fundUser}","brl":${__P(fund_brl,100000000000)},"vib":${__P(fund_vib,100000000)}}'),
                           node(jsr223("JSR223PreProcessor", "Usuario a fondear", PRE_FUND_USER, "pre_fund"), "") +
                           node(jsr223("JSR223Assertion", "204",
                                       COMMON + "if (code != '204') fail('depósito: esperado 204, recibido ' + code + ' ' + text)\n", "assert_204"), ""))))

    main = node(thread_group("Carga de órdenes", "${__P(threads,128)}", "${__P(rampup,30)}", "${__P(duration,60)}"), main_children)

    teardown = node(thread_group("Verificación final", "1", "0", "0", loops="1", scheduler=False, cls="PostThreadGroup"),
                    header_manager() + ht() +
                    node(http_sampler("TEARDOWN GET /audit (conservación)", "GET", "/audit"),
                         node(jsr223("JSR223Assertion", "Invariantes ok y valor conservado", ASSERT_AUDIT_FINAL, "assert_audit_final"), "")))

    plan = test_plan("Vibranium Order Book - CARGA",
                     "Plan de carga. Orden: línea base -> fondeo -> carga -> auditoría de conservación. "
                     "Todo se parametriza con -J (host, port, threads, rampup, duration, tps, users, mid_price, spread, cancel_pct, with_history).",
                     [])
    inner = (http_defaults() + ht() + setup + main + teardown)
    return xml(node(plan, inner))


# ---------------------------------------------------------------- plan de INTEGRIDAD FINANCIERA (determinista, 1 hilo)
def integrity_plan():
    steps = []

    def step(name, method, path, body, script, key, post=None, pre=None):
        kids = ""
        if pre:
            kids += node(jsr223("JSR223PreProcessor", f"pre {name}", pre, f"pre_{key}"), "")
        kids += node(jsr223("JSR223Assertion", f"Validar {name}", script, f"assert_{key}"), "")
        if post:
            kids += node(jsr223("JSR223PostProcessor", f"post {name}", post, f"post_{key}"), "")
        steps.append(node(http_sampler(name, method, path, body), kids))

    HDR = COMMON + "def J = { new JsonSlurper().parseText(text) }\n"
    eq = lambda cond, msg: f'if (!({cond})) {{ fail("{msg} -> " + text); return }}\n'

    pre_ids = r"""
String run = props.getProperty('run_id')
vars.put('S', "jm${run}-vendedor"); vars.put('B', "jm${run}-comprador"); vars.put('P', "jm${run}-pobre")
vars.put('o1', "jm${run}-o1"); vars.put('o2', "jm${run}-o2"); vars.put('o3', "jm${run}-o3")
"""
    P = "long p0 = vars.get('P0') as long; long brl0 = vars.get('BRL0') as long\n"
    steps.append(node(http_sampler("00 GET /healthz", "GET", "/healthz"),
                      node(jsr223("JSR223PreProcessor", "Fijar run_id, nombres y precios", PRE_RUNID + pre_ids, "pre_ids"), "") +
                      node(jsr223("JSR223Assertion", "200", COMMON + "if (code != '200') fail('healthz ' + code)\n", "a_health"), "")))
    step("01 GET /readyz", "GET", "/readyz", None, COMMON + "if (code != '200') fail('readyz ' + code + ' ' + text)\n", "ready")
    step("01b GET /book: elegir una banda de precios sin liquidez ajena", "GET", "/book?depth=100", None, HDR_BAND, "band")
    step("02 GET /audit (línea base)", "GET", "/audit", None, HDR + eq("code == '200' && J().ok == true", "auditoría inicial no ok"), "a0",
         post="def a = new groovy.json.JsonSlurper().parseText(prev.getResponseDataAsString()); props.setProperty('i_base_brl', a.total_brl.toString()); props.setProperty('i_base_vib', a.total_vib.toString())")
    step("03 POST /deposits vendedor: 100 VIB", "POST", "/deposits", '{"user_id":"${S}","brl":0,"vib":100}', COMMON + "if (code != '204') fail('esperado 204, recibido ' + code + ' ' + text)\n", "d1")
    step("04 POST /deposits comprador: 20 x P1 centavos", "POST", "/deposits", '{"user_id":"${B}","brl":${BRL0},"vib":0}', COMMON + "if (code != '204') fail('esperado 204, recibido ' + code + ' ' + text)\n", "d2")
    step("05 SELL 10 @ P0 (queda en el libro)", "POST", "/orders", '{"id":"${o1}","user_id":"${S}","side":"SELL","price":${P0},"qty":10}',
         HDR + eq("code == '201'", "esperado 201") + "def r = J()\n" + eq("r.order.status == 'OPEN' && r.order.filled == 0 && r.order.remaining == 10 && r.trades.size() == 0", "debe quedar OPEN sin trades"), "s1")
    step("06 saldo vendedor: 10 VIB reservadas", "GET", "/balances/${S}", None,
         HDR + "def b = J()\n" + eq("b.vib_free == 90 && b.vib_held == 10 && b.brl_free == 0 && b.brl_held == 0", "reserva de venta incorrecta"), "b1")
    step("07 BUY 4 @ P0+100 cruza al precio del MAKER (P0)", "POST", "/orders", '{"id":"${o2}","user_id":"${B}","side":"BUY","price":${P1},"qty":4}',
         HDR + P + eq("code == '201'", "esperado 201") + "def r = J()\n" +
         eq("r.order.status == 'FILLED' && r.order.filled == 4 && r.trades.size() == 1", "debe ejecutarse completa con 1 trade") +
         eq("r.trades[0].price == p0 && r.trades[0].qty == 4 && r.trades[0].total == 4 * p0 && r.trades[0].maker_order_id == vars.get('o1') && r.trades[0].taker_order_id == vars.get('o2')",
            "el trade debe ser a P0 (precio del maker), 4 unidades, total 4*P0"), "s2")
    step("08 saldo comprador: pagó 4*P0 (se devolvió la mejora de precio)", "GET", "/balances/${B}", None,
         HDR + P + "def b = J()\n" + eq("b.brl_free == brl0 - 4 * p0 && b.brl_held == 0 && b.vib_free == 4 && b.vib_held == 0", "liquidación del comprador incorrecta"), "b2")
    step("09 saldo vendedor: cobró 4*P0, quedan 6 VIB reservadas", "GET", "/balances/${S}", None,
         HDR + P + "def b = J()\n" + eq("b.brl_free == 4 * p0 && b.vib_free == 90 && b.vib_held == 6", "liquidación del vendedor incorrecta"), "b3")
    step("10 BUY 10 @ P0: parcial (6 ejecutadas, 4 en el libro)", "POST", "/orders", '{"id":"${o3}","user_id":"${B}","side":"BUY","price":${P0},"qty":10}',
         HDR + eq("code == '201'", "esperado 201") + "def r = J()\n" +
         eq("r.order.status == 'PARTIALLY_FILLED' && r.order.filled == 6 && r.order.remaining == 4 && r.trades.size() == 1 && r.trades[0].qty == 6", "debe ser parcial: 6 de 10"), "s3")
    step("11 libro: compra 4 @ P0 y ninguna venta a P0 o menos", "GET", "/book?depth=100", None,
         HDR + P + "def b = J()\n" + eq("b.bids.any { it.price == p0 && it.qty >= 4 } && b.asks.every { it.price > p0 }", "libro incoherente"), "bk")
    step("12 saldo comprador: 4*P0 reservadas por el remanente", "GET", "/balances/${B}", None,
         HDR + P + "def b = J()\n" + eq("b.brl_free == brl0 - 4 * p0 - 10 * p0 && b.brl_held == 4 * p0 && b.vib_free == 10", "reserva del remanente incorrecta"), "b4")
    step("13 DELETE de la orden parcial: libera lo reservado", "DELETE", "/orders/${o3}", None,
         HDR + eq("code == '200'", "esperado 200") + "def r = J()\n" + eq("r.status == 'CANCELLED' && r.filled == 6", "debe quedar CANCELLED con 6 ejecutadas"), "c1")
    step("14 saldo comprador tras cancelar", "GET", "/balances/${B}", None,
         HDR + P + "def b = J()\n" + eq("b.brl_held == 0 && b.brl_free == brl0 - 10 * p0 && b.vib_free == 10", "no se liberó la reserva"), "b5")
    step("15 DELETE otra vez -> 404", "DELETE", "/orders/${o3}", None, expect("404", "order_not_found"), "c2")
    step("16 reenviar el mismo id -> 409 (idempotencia)", "POST", "/orders", '{"id":"${o2}","user_id":"${B}","side":"BUY","price":${P1},"qty":4}', expect("409", "duplicate_order"), "e409")
    step("17 usuario sin fondos -> 422", "POST", "/orders", '{"user_id":"${P}","side":"BUY","price":${P0},"qty":1}', expect("422", "insufficient_funds"), "e422")
    step("18 lado inválido -> 400", "POST", "/orders", '{"user_id":"${B}","side":"HOLD","price":${P0},"qty":1}', expect("400", "invalid_side"), "e400a")
    step("19 id inválido -> 400", "POST", "/orders", '{"id":"con espacio","user_id":"${B}","side":"BUY","price":${P0},"qty":1}', expect("400", "invalid_id"), "e400b")
    step("20 precio decimal -> 400 (solo enteros)", "POST", "/orders", '{"user_id":"${B}","side":"BUY","price":10.5,"qty":1}', expect("400", "invalid_json"), "e400c")
    step("21 cantidad 0 -> 400", "POST", "/orders", '{"user_id":"${B}","side":"BUY","price":${P0},"qty":0}', expect("400", "invalid_request"), "e400d")
    step("22 depth=101 -> 400", "GET", "/book?depth=101", None, expect("400", "invalid_depth"), "e400e")
    step("23 GET /audit: invariantes y conservación", "GET", "/audit", None,
         HDR + eq("code == '200' && J().ok == true", "invariantes rotos") + "def a = J()\n" +
         'long eb = (props.getProperty("i_base_brl") as long) + (vars.get("BRL0") as long); long ev = (props.getProperty("i_base_vib") as long) + 100\n' +
         'if (a.total_brl != eb || a.total_vib != ev) fail("conservación rota: BRL ${a.total_brl} (esperado ${eb}) VIB ${a.total_vib} (esperado ${ev})")\n', "a1")
    hist = node(if_controller("Solo con historial (-Jwith_history=true)", "${__groovy(props.getProperty('with_history') == 'true')}"),
                node(while_controller("Esperar a que el historial alcance al motor (máx. 20 intentos)",
                                      "${__groovy(vars.get('consistent') != 'true' && ((vars.get('tries') ?: '0') as int) < 20)}"),
                     node(http_sampler("24 GET /audit/history/{comprador}", "GET", "/audit/history/${B}"),
                          node(jsr223("JSR223PostProcessor", "¿consistente?",
                                      "def r = new groovy.json.JsonSlurper().parseText(prev.getResponseDataAsString()); vars.put('consistent', (r.conclusive == true && r.consistent == true) ? 'true' : 'false'); vars.put('tries', (((vars.get('tries') ?: '0') as int) + 1).toString())",
                                      "post_hist"), "")) +
                     node(test_action_pause("pausa 500 ms", "500"), "")) +
                node(http_sampler("25 verificación final del historial", "GET", "/audit/history/${B}"),
                     node(jsr223("JSR223Assertion", "conclusive && consistent",
                                 HDR + "def r = J()\n" + eq("code == '200' && r.conclusive == true && r.consistent == true", "el saldo del historial no coincide con el del motor"), "assert_hist"), "")))
    steps.append(hist)

    tg = node(thread_group("Integridad financiera (1 hilo, determinista)", "1", "0", "0", loops="1", scheduler=False),
              header_manager() + ht() + "".join(steps))
    plan = test_plan("Vibranium Order Book - INTEGRIDAD FINANCIERA",
                     "Escenario determinista de un solo hilo: valida liquidación exacta, precio del maker, parciales, reservas, cancelación, idempotencia, "
                     "errores del contrato y conservación de valor. NO es una prueba de carga. Parámetros: host, port, protocol, with_history, run_id.",
                     [])
    return xml(node(plan, http_defaults() + ht() + tg))


def xml(root_inner):
    return ('<?xml version="1.0" encoding="UTF-8"?>\n<jmeterTestPlan version="1.2" properties="5.0" jmeter="5.6.3">'
            + ht(root_inner) + "</jmeterTestPlan>\n")


if __name__ == "__main__":
    (HERE / "vibranium-load.jmx").write_text(load_plan(), encoding="utf-8")
    (HERE / "vibranium-integrity.jmx").write_text(integrity_plan(), encoding="utf-8")
    print("escritos: vibranium-load.jmx, vibranium-integrity.jmx")
