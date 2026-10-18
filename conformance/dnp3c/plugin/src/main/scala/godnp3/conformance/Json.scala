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

// The little JSON the plugin speaks to the device under test: enough to send
// arguments and read a reply, so the plugin needs nothing beyond dnp3c.
enum Json {
  case Null
  case Bool(value: Boolean)
  case Num(value: Double)
  case Str(value: String)
  case Arr(items: Seq[Json])
  case Obj(fields: Seq[(String, Json)])

  def render: String = this match {
    case Null => "null"
    case Bool(b) => b.toString
    case Num(n) => if (n.isWhole && math.abs(n) < 1e15) n.toLong.toString else n.toString
    case Str(s) => Json.quote(s)
    case Arr(items) => items.map(_.render).mkString("[", ",", "]")
    case Obj(fields) => fields.map((k, v) => s"${Json.quote(k)}:${v.render}").mkString("{", ",", "}")
  }

  def field(name: String): Json = this match {
    case Obj(fields) => fields.collectFirst { case (`name`, v) => v }.getOrElse(Null)
    case _ => Null
  }
}

object Json {
  def obj(fields: (String, Json)*): Json = Obj(fields)
  def num(n: Long): Json = Num(n.toDouble)
  def opt(n: Option[Long]): Json = n.fold(Null)(num)

  private def quote(s: String): String = {
    val b = new StringBuilder("\"")
    s.foreach {
      case '"' => b ++= "\\\""
      case '\\' => b ++= "\\\\"
      case '\n' => b ++= "\\n"
      case '\r' => b ++= "\\r"
      case '\t' => b ++= "\\t"
      case c if c < ' ' => b ++= f"\\u${c.toInt}%04x"
      case c => b += c
    }
    b += '"'
    b.toString
  }

  def parse(s: String): Json = {
    val p = new Parser(s)
    val v = p.value()
    p.end()
    v
  }

  private final class Parser(s: String) {
    private var i = 0

    private def fail(what: String): Nothing = throw new IllegalArgumentException(s"bad JSON at $i: $what")
    private def ws(): Unit = while (i < s.length && s(i).isWhitespace) i += 1
    private def peek: Char = { ws(); if (i < s.length) s(i) else fail("unexpected end") }
    private def expect(c: Char): Unit = if (peek == c) i += 1 else fail(s"expected '$c'")
    private def literal(word: String, v: Json): Json =
      if (s.startsWith(word, i)) { i += word.length; v }
      else fail(s"expected $word")

    def end(): Unit = { ws(); if (i != s.length) fail("trailing data") }

    def value(): Json = peek match {
      case '{' =>
        i += 1
        val fields = Seq.newBuilder[(String, Json)]
        if (peek == '}') i += 1
        else {
          var more = true
          while (more) {
            val k = string()
            expect(':')
            fields += k -> value()
            if (peek == ',') i += 1 else { expect('}'); more = false }
          }
        }
        Obj(fields.result())
      case '[' =>
        i += 1
        val items = Seq.newBuilder[Json]
        if (peek == ']') i += 1
        else {
          var more = true
          while (more) {
            items += value()
            if (peek == ',') i += 1 else { expect(']'); more = false }
          }
        }
        Arr(items.result())
      case '"' => Str(string())
      case 't' => literal("true", Bool(true))
      case 'f' => literal("false", Bool(false))
      case 'n' => literal("null", Null)
      case _ =>
        val start = i
        while (i < s.length && "+-0123456789.eE".contains(s(i))) i += 1
        s.substring(start, i).toDoubleOption.map(Num(_)).getOrElse(fail("expected a value"))
    }

    private def string(): String = {
      expect('"')
      val b = new StringBuilder
      while (s(i) != '"') {
        if (s(i) == '\\') {
          i += 1
          s(i) match {
            case 'n' => b += '\n'
            case 'r' => b += '\r'
            case 't' => b += '\t'
            case 'b' => b += '\b'
            case 'f' => b += '\f'
            case 'u' => b += Integer.parseInt(s.substring(i + 1, i + 5), 16).toChar; i += 4
            case c => b += c
          }
        } else b += s(i)
        i += 1
      }
      i += 1
      b.toString
    }
  }
}
