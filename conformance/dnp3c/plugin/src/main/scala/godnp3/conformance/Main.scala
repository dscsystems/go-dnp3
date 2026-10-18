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

import io.stepfunc.dnp3c.conformance.{ Harness, Revisions }
import io.stepfunc.dnp3c.conformance.api.{ HarnessExecutionException, Options, ProcedureOutcome, ProcedureResult }

import java.net.URI
import java.nio.file.{ Files, Paths }
import scala.util.control.NonFatal

/**
 * dnp3c's command-line tool with the go-dnp3 plugin performing its steps. The device under test's control address
 * comes from DUT_CONTROL.
 *
 * With -keep-going first, it runs `-rev <rev> -config <file> -run <id>` past failures: dnp3c stops a run at the first
 * procedure that fails, which leaves every later procedure unreached, so the rest are run one at a time. It prints
 * each outcome, writes them to summary.txt next to the report, and exits 1 if any procedure failed.
 *
 * Without it, the arguments are dnp3c's own, and it exits 0 when every procedure passed, was skipped or was excluded.
 */
object Main {
  def main(args: Array[String]): Unit = {
    val control = sys.env.getOrElse("DUT_CONTROL", "http://127.0.0.1:20080")
    val plugin = new GoDnp3Plugin(URI.create(control))
    args.toList match {
      case "-keep-going" :: rest => sys.exit(keepGoing(rest, plugin))
      case _ => io.stepfunc.dnp3c.conformance.Main.run(args, plugin)
    }
  }

  private def keepGoing(args: List[String], plugin: GoDnp3Plugin): Int = {
    val opts = args.grouped(2).collect { case List(k, v) => k -> v }.toMap
    def arg(name: String) = opts.getOrElse(name, throw new IllegalArgumentException(s"$name is required"))
    val revision = Revisions.find(arg("-rev")).getOrElse(throw new IllegalArgumentException("unknown revision"))
    val harness = new Harness(Options.load(Paths.get(arg("-config"))), plugin, revision)

    val results = List.newBuilder[ProcedureResult]
    var pending = List(opts.getOrElse("-run", "all"))
    var first = true
    while (pending.nonEmpty) {
      val id = pending.head
      pending = pending.tail
      try {
        val summary = harness.run(id)
        results ++= summary.results
        // only the first run, of the whole selection, leaves procedures unreached
        if (first) pending = summary.unreached
      } catch {
        case e: HarnessExecutionException =>
          results ++= e.partial.results
          results += ProcedureResult(e.procedureId, ProcedureOutcome.Failed(s"harness: ${e.getMessage}"))
          if (first) pending = e.partial.unreached.filterNot(_ == e.procedureId)
      }
      first = false
    }

    val all = results.result()
    val lines = all.map { r =>
      val outcome = r.outcome match {
        case ProcedureOutcome.Passed => if (r.warnings.isEmpty) "PASSED" else s"PASSED with warnings: ${r.warnings.mkString("; ")}"
        case ProcedureOutcome.Failed(m) => s"FAILED: $m"
        case ProcedureOutcome.Skipped(m) => s"skipped: $m"
        case ProcedureOutcome.Excluded(m) => s"EXCLUDED: $m"
      }
      s"${r.id}\t$outcome"
    }
    def count(p: ProcedureOutcome => Boolean) = all.count(r => p(r.outcome))
    val failed = count(_.isInstanceOf[ProcedureOutcome.Failed])
    val totals = s"passed ${count(_ == ProcedureOutcome.Passed)}, failed $failed, " +
      s"excluded ${count(_.isInstanceOf[ProcedureOutcome.Excluded])}, skipped ${count(_.isInstanceOf[ProcedureOutcome.Skipped])}"

    val report =
      try Some(harness.writeReport())
      catch {
        case NonFatal(e) =>
          System.err.println(s"Failed to write the report: ${e.getMessage}")
          None
      }
    report.foreach { path =>
      Files.writeString(path.resolveSibling("summary.txt"), (lines :+ totals).mkString("", "\n", "\n"))
    }
    println()
    lines.foreach(println)
    println(totals)
    if (failed > 0 || report.isEmpty) 1 else 0
  }
}
