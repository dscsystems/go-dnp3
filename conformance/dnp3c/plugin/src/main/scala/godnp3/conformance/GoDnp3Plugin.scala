// Copyright (C) 2026 Ricardo Olsen / DSC Systems.
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU General Public License as published by the Free
// Software Foundation, either version 3 of the License, or (at your option)
// any later version. It is distributed WITHOUT ANY WARRANTY; without even the
// implied warranty of MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.
// See the GNU General Public License for more details, in the LICENSE file at
// the root of this repository or at <https://www.gnu.org/licenses/>.

package godnp3.conformance

import io.stepfunc.dnp3c.dnp3._
import io.stepfunc.dnp3c.dnp3.app._
import io.stepfunc.dnp3c.dnp3.app.objects.AttributeValue

import java.net.URI
import java.net.http.{ HttpClient, HttpRequest, HttpResponse }
import java.time.Duration

/**
 * dnp3c's integration plugin for go-dnp3, as a shim: every call goes to the device under test (../dut) as one JSON
 * request, which acts on the outstation or checks what dnp3c parsed. The DUT's log lines go into the procedure's log,
 * and its error, if any, fails the step.
 */
final class GoDnp3Plugin(control: URI) extends IntegrationPlugin {

  private val http = HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(5)).build()

  private def call(reporter: PluginReporter, method: String, args: (String, Json)*): Json = {
    val body = Json.obj("method" -> Json.Str(method), "args" -> Json.Obj(args)).render
    val request = HttpRequest
      .newBuilder(control.resolve("/rpc"))
      .timeout(Duration.ofSeconds(30))
      .header("Content-Type", "application/json")
      .POST(HttpRequest.BodyPublishers.ofString(body))
      .build()
    val response = http.send(request, HttpResponse.BodyHandlers.ofString())
    if (response.statusCode() != 200)
      throw new Exception(s"device under test: HTTP ${response.statusCode()}: ${response.body()}")
    val reply = Json.parse(response.body())
    reply.field("log") match {
      case Json.Arr(lines) => lines.foreach { case Json.Str(l) => reporter.log(l); case _ => }
      case _ =>
    }
    reply.field("error") match {
      case Json.Str(e) if e.nonEmpty => throw new Exception(e)
      case _ => reply.field("result")
    }
  }

  // a check that has run: the DUT checks at once, never later
  private def check(reporter: CheckReporter, method: String, args: (String, Json)*): Option[DeferredCheck] = {
    call(reporter, method, args*)
    None
  }

  // ---------- Encoding ----------

  private def kindName(kind: PointKind): String = kind match {
    case PointKind.BinaryInput => "binary"
    case PointKind.DoubleBitInput => "doubleBit"
    case PointKind.Counter => "counter"
    case PointKind.FrozenCounter => "frozenCounter"
    case PointKind.AnalogInput => "analog"
    case PointKind.BinaryOutputStatus => "binaryOutputStatus"
    case PointKind.AnalogOutputStatus => "analogOutputStatus"
  }

  private def kind(k: PointKind): (String, Json) = "kind" -> Json.Str(kindName(k))

  private def eventClass(c: EventClass): (String, Json) = "class" -> Json.num(c match {
    case EventClass.Class1 => 1
    case EventClass.Class2 => 2
    case EventClass.Class3 => 3
    case _ => 0
  })

  private def point(p: GenIndexedPoint[? <: MeasurementPoint]): Json = {
    val (k, value) = p.point match {
      case b: BinaryPoint => ("binary", if (b.value) 1.0 else 0.0)
      case d: DoubleBitPoint => ("doubleBit", d.value.value.toDouble)
      case c: CounterPoint => (if (c.isFrozen) "frozenCounter" else "counter", c.value.toDouble)
      case a: AnalogPoint => (if (a.isFrozen) "frozenAnalog" else "analog", a.value)
      case b: BinaryOutputStatusPoint => ("binaryOutputStatus", if (b.value) 1.0 else 0.0)
      case a: AnalogOutputStatusPoint => ("analogOutputStatus", a.value)
    }
    Json.obj(
      "kind" -> Json.Str(k),
      "index" -> Json.num(p.idx),
      "value" -> Json.Num(value),
      "flags" -> Json.opt(p.point.flags.map(f => (f & 0xFF).toLong)),
      "time" -> Json.opt(p.point.timestamp.map(_.value)),
      "event" -> Json.Bool(p.point.pointType == PointType.Event)
    )
  }

  private def points(name: String, ps: Seq[GenIndexedPoint[? <: MeasurementPoint]]): (String, Json) =
    name -> Json.Arr(ps.map(point))

  private def attribute(variation: Int, value: AttributeValue): Json = {
    val (t, text, number) = value match {
      case AttributeValue.VisibleString(s) => ("vstr", s, 0.0)
      case AttributeValue.UnsignedInt(n) => ("uint", "", n.toDouble)
      case AttributeValue.SignedInt(n) => ("int", "", n.toDouble)
      case AttributeValue.FloatingPoint(n) => ("flt", "", n)
      case other => ("other", other.description, 0.0)
    }
    Json.obj(
      "variation" -> Json.num(variation),
      "type" -> Json.Str(t),
      "text" -> Json.Str(text),
      "number" -> Json.Num(number))
  }

  // ---------- Procedure ----------

  override def startProcedure(reporter: PluginReporter): Unit = call(reporter, "startProcedure")
  override def endProcedure(reporter: PluginReporter): Unit = call(reporter, "endProcedure")

  // ---------- Actions ----------

  override def cyclePower(reporter: PluginReporter): Unit = call(reporter, "cyclePower")
  override def setColdRestartSupport(reporter: PluginReporter, enabled: Boolean): Unit =
    call(reporter, "setColdRestartSupport", "value" -> Json.Bool(enabled))

  override def setLinkLayerConfirm(reporter: PluginReporter, value: Boolean): Unit =
    call(reporter, "setLinkLayerConfirm", "value" -> Json.Bool(value))

  override def setSelfAddressSupport(reporter: PluginReporter, value: Boolean): Unit =
    call(reporter, "setSelfAddressSupport", "value" -> Json.Bool(value))

  override def checkOperate(reporter: CheckReporter, k: OutputKind, index: Int): Option[DeferredCheck] =
    check(reporter, "checkOperate", kind(k), "index" -> Json.num(index))

  override def checkNotOperate(reporter: CheckReporter, k: OutputKind): Option[DeferredCheck] =
    check(reporter, "checkNotOperate", kind(k))

  override def generateInputChanges(reporter: PluginReporter, k: InputKind[?]): Unit =
    call(reporter, "generateInputChanges", kind(k))

  override def generateExactInputEvents(reporter: PluginReporter, k: InputKind[?], numEvents: Int): Unit =
    call(reporter, "generateExactInputEvents", kind(k), "count" -> Json.num(numEvents))

  override def generateClassEvents(reporter: PluginReporter, c: EventClass): Unit =
    call(reporter, "generateClassEvents", eventClass(c))

  override def generateExactClassEvents(reporter: PluginReporter, c: EventClass, numEvents: Int): Unit =
    call(reporter, "generateExactClassEvents", eventClass(c), "count" -> Json.num(numEvents))

  override def generatePattern(reporter: PluginReporter, k: InputKind[?]): Unit =
    call(reporter, "generatePattern", kind(k))

  override def uninstall(reporter: PluginReporter, k: PointKind): Unit = call(reporter, "uninstall", kind(k))

  override def reinstallPoints(reporter: PluginReporter): Unit = call(reporter, "reinstallPoints")

  override def enableRemoteSupervisoryControl(reporter: PluginReporter): Unit =
    call(reporter, "enableRemoteSupervisoryControl")

  override def setGlobalRemoteSupervisoryControl(reporter: PluginReporter): Unit =
    call(reporter, "setGlobalRemoteSupervisoryControl")

  override def setIndividualRemoteSupervisoryControl(reporter: PluginReporter, index: Int): Unit =
    call(reporter, "setIndividualRemoteSupervisoryControl", "index" -> Json.num(index))

  override def enableUnsolicitedResponse(reporter: PluginReporter, enabled: Boolean): Unit =
    call(reporter, "enableUnsolicitedResponse", "value" -> Json.Bool(enabled))

  override def setUnsolicitedResponseTimeout(reporter: PluginReporter, timeoutMs: Int): Unit =
    call(reporter, "setUnsolicitedResponseTimeout", "timeoutMs" -> Json.num(timeoutMs))

  override def setMaxUnsolicitedRetries(reporter: PluginReporter, numRetries: Option[Int]): Unit =
    call(reporter, "setMaxUnsolicitedRetries", "retries" -> Json.opt(numRetries.map(_.toLong)))

  override def setMasterAddress(reporter: PluginReporter, address: Int): Unit =
    call(reporter, "setMasterAddress", "address" -> Json.num(address))

  // the DUT generates the events before it answers, whatever the timing allows
  override def generateUnsolicitedEvents(reporter: PluginReporter, timing: ActionTiming): ActionResult = {
    call(reporter, "generateUnsolicitedEvents")
    ActionResult.Performed
  }

  override def generateNotEnoughUnsolicitedEvents(reporter: PluginReporter): Unit =
    call(reporter, "generateNotEnoughUnsolicitedEvents")

  override def recordCurrentCounterValues(reporter: PluginReporter): Unit =
    call(reporter, "recordCurrentCounterValues")

  override def generateTimeEvent(reporter: PluginReporter): Unit = call(reporter, "generateTimeEvent")

  override def setMaxFragmentSize(reporter: PluginReporter, maxFragmentSize: Int): Unit =
    call(reporter, "setMaxFragmentSize", "size" -> Json.num(maxFragmentSize))

  override def generateMultiFragmentResponse(reporter: PluginReporter): Unit =
    call(reporter, "generateMultiFragmentResponse")

  override def setAnalogInput(reporter: PluginReporter, index: Int, value: Double): Unit =
    call(reporter, "setAnalogInput", "index" -> Json.num(index), "analogValue" -> Json.Num(value))

  override def generateExactFrozenCounterEvents(reporter: PluginReporter, numEvents: Int): Unit =
    call(reporter, "generateExactFrozenCounterEvents", "count" -> Json.num(numEvents))

  override def generateExactSmallAnalogInputEvents(reporter: PluginReporter, numEvents: Int): Unit =
    call(reporter, "generateExactSmallAnalogInputEvents", "count" -> Json.num(numEvents))

  override def generateEventOn(reporter: PluginReporter, k: PointKind, index: Int): Unit =
    call(reporter, "generateEventOn", kind(k), "index" -> Json.num(index))

  override def changeOutputFlag(reporter: PluginReporter, k: PointKind, index: Int, flag: Int): Unit =
    call(reporter, "changeOutputFlag", kind(k), "index" -> Json.num(index), "flag" -> Json.num(flag))

  override def changeBinaryOutputState(reporter: PluginReporter, index: Int): Unit =
    call(reporter, "changeBinaryOutputState", "index" -> Json.num(index))

  override def setAnalogOutputValue(reporter: PluginReporter, index: Int, value: Double): Unit =
    call(reporter, "setAnalogOutputValue", "index" -> Json.num(index), "analogValue" -> Json.Num(value))

  override def setFlagsNormal(reporter: PluginReporter, k: PointKind): Unit = call(reporter, "setFlagsNormal", kind(k))

  override def setFlagAbnormal(reporter: PluginReporter, k: PointKind, indices: Seq[Int]): AbnormalFlag = {
    val result = call(reporter, "setFlagAbnormal", kind(k), "indices" -> Json.Arr(indices.map(i => Json.num(i))))
    (result.field("index"), result.field("flags")) match {
      case (Json.Num(i), Json.Num(f)) => AbnormalFlag(i.toInt, Some(f.toInt))
      case _ => throw new Exception(s"device under test: unexpected reply to setFlagAbnormal: ${result.render}")
    }
  }

  // ---------- Checks ----------

  override def verifyAllStaticCurrentValues(
      reporter: CheckReporter,
      isClass0: Boolean,
      ps: Seq[GenIndexedPoint[MeasurementPoint]]): Option[DeferredCheck] =
    check(reporter, "verifyAllStaticCurrentValues", "isClass0" -> Json.Bool(isClass0), points("points", ps))

  override def verifyInputCurrentValues[P <: MeasurementPoint](
      reporter: CheckReporter,
      k: InputKind[P],
      ps: Seq[GenIndexedPoint[P]]): Option[DeferredCheck] =
    check(reporter, "verifyInputCurrentValues", kind(k), points("points", ps))

  override def verifyRecordedCounterValues(
      reporter: CheckReporter,
      ps: Seq[GenIndexedPoint[CounterPoint]]): Option[DeferredCheck] =
    check(reporter, "verifyRecordedCounterValues", points("points", ps))

  override def verifyInputEvents[P <: MeasurementPoint](
      reporter: CheckReporter,
      k: InputKind[P],
      expectation: EventExpectation,
      ps: Seq[GenIndexedPoint[P]]): Option[DeferredCheck] =
    check(
      reporter,
      "verifyInputEvents",
      kind(k),
      "expectation" -> Json.Str(expectation.toString),
      points("points", ps))

  override def verifyFirstInputEvent[P <: MeasurementPoint](
      reporter: CheckReporter,
      k: InputKind[P],
      p: GenIndexedPoint[P]): Option[DeferredCheck] =
    check(reporter, "verifyFirstInputEvent", kind(k), points("points", Seq(p)))

  override def verifyClassEvents(
      reporter: CheckReporter,
      c: EventClass,
      expectation: EventExpectation,
      ps: Seq[GenIndexedPoint[MeasurementPoint]]): Option[DeferredCheck] =
    check(
      reporter,
      "verifyClassEvents",
      eventClass(c),
      "expectation" -> Json.Str(expectation.toString),
      points("points", ps))

  override def verifyFirstClassEvent(
      reporter: CheckReporter,
      c: EventClass,
      p: GenIndexedPoint[MeasurementPoint]): Option[DeferredCheck] =
    check(reporter, "verifyFirstClassEvent", eventClass(c), points("points", Seq(p)))

  override def verifyFrozenCounterEvents(
      reporter: CheckReporter,
      ps: Seq[GenIndexedPoint[CounterPoint]]): Option[DeferredCheck] =
    check(reporter, "verifyFrozenCounterEvents", points("points", ps))

  override def checkAnalogOutputTolerance(
      reporter: CheckReporter,
      expectedValue: Double,
      actualValue: Double): Option[DeferredCheck] =
    check(
      reporter,
      "checkAnalogOutputTolerance",
      "expected" -> Json.Num(expectedValue),
      "actual" -> Json.Num(actualValue))

  override def verifyPolledDataDoNotRepeatUnsolicitedData(
      reporter: CheckReporter,
      unsolicited: Seq[GenIndexedPoint[MeasurementPoint]],
      polled: Seq[GenIndexedPoint[MeasurementPoint]]): Option[DeferredCheck] =
    check(
      reporter,
      "verifyPolledDataDoNotRepeatUnsolicitedData",
      points("previous", unsolicited),
      points("points", polled))

  override def verifyResponseRetainsEvents(
      reporter: CheckReporter,
      previous: Seq[GenIndexedPoint[MeasurementPoint]],
      current: Seq[GenIndexedPoint[MeasurementPoint]]): Option[DeferredCheck] =
    check(reporter, "verifyResponseRetainsEvents", points("previous", previous), points("points", current))

  override def verifyCommandEventTime(reporter: CheckReporter, sentMs: Long, eventMs: Long): Option[DeferredCheck] =
    check(reporter, "verifyCommandEventTime", "sentMs" -> Json.num(sentMs), "eventMs" -> Json.num(eventMs))

  override def verifyDelayMeasurementAccuracy(reporter: CheckReporter, delayMs: Long): Option[DeferredCheck] =
    check(reporter, "verifyDelayMeasurementAccuracy", "delayMs" -> Json.num(delayMs))

  override def verifyTimestamp(
      reporter: CheckReporter,
      ps: Seq[GenIndexedPoint[MeasurementPoint]]): Option[DeferredCheck] =
    check(reporter, "verifyTimestamp", points("points", ps))

  override def verifyDeviceAttributes(
      reporter: CheckReporter,
      attributes: Seq[(Int, AttributeValue)]): Option[DeferredCheck] =
    check(reporter, "verifyDeviceAttributes", "attributes" -> Json.Arr(attributes.map(attribute)))
}
