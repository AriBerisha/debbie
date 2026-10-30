# Third-party notices

debbie ports selected logic and prompt text from Qwen Code, which is itself
derived from Google's Gemini CLI. Both are licensed under the Apache License,
Version 2.0, reproduced in full below.

Nothing from these projects is imported, vendored or bulk-copied. Each item listed
here was studied in the source and rewritten in Go, or adapted as prompt text, and
modified for this program.

## Qwen Code

- Source: https://github.com/QwenLM/qwen-code
- Studied at commit `70be140e148f8fc7527cc51717603a6dd84abc19` (2026-10-30)
- Copyright 2025 Google LLC (Gemini CLI); Copyright 2025-2026 Qwen Team
- License: Apache License 2.0

### Ported items

Each entry: the debbie file, the source path in Qwen Code, what was taken, and
how it was changed.

| debbie file | Source path | What was taken | Changes |
|---|---|---|---|
| `internal/effort/effort.go` | `packages/core/src/core/reasoning-effort.ts` | The reasoning-effort ladder (low, medium, high, xhigh, max) and its numeric ranks, the aliases accepted when parsing a level, and the rule for fitting a level to a model (keep a supported level, else the next stronger one, else the strongest) | Rewritten in Go. Added `none` and `minimal`; the supported levels come from llama-server's `/props` instead of a per-model table |
| `internal/tools/editmatch.go`, `internal/tools/edit.go` | `packages/core/src/utils/editHelper.ts`, `packages/core/src/tools/edit.ts` | The order of the edit tool's matching: the exact text, then with typographic lookalike characters (dashes, curly quotes, unusual spaces) made plain, using the same character table, then line by line ignoring trailing whitespace, never relaxing indentation; taking a deleted line's newline along; showing the changed lines with four lines of context in the result | Rewritten in Go with byte offsets. Every loose match is counted, so a second match makes the edit ambiguous instead of the first one winning. Added last resorts (consistently shifted indentation, over-escaped text, line numbers copied from `read_file`) and the closest-lines hint for a failed match |
| `internal/agent/loop.go`, `internal/agent/session.go` | `packages/core/src/services/loopDetectionService.ts` | The check for replies that chant: 50-character chunks tracked over the last 4000 characters, a loop when a chunk occurs 10 times with an average spacing of at most 75 characters, or when at least 5 equally spaced occurrences (3 once the window is full) span 1000 or more characters that repeat exactly; Markdown structure resets the tracking and code blocks are skipped, while reasoning skips the Markdown rules. Also the idea of keying calls by tool name and canonical arguments, and of appending a warning to the next tool result | Rewritten in Go over bytes rather than UTF-16 units, with `hash/maphash` instead of SHA-256 and one detector per reply. The thresholds for identical calls follow SPEC §7 (a warning at 3, a stop at 4); the warning text is new |
| `internal/prompt/tools/shell_cmd.md`, `internal/prompt/tools/shell_powershell.md` | `packages/core/src/tools/shell.ts` | The shell tool's guidance on quoting and chaining commands in cmd.exe and PowerShell | Reworded and shortened |
| `internal/tools/shell_windows.go` | `packages/core/src/services/shellExecutionService.ts` | Making Windows shells write UTF-8: `chcp 65001`, by absolute path, before cmd.exe commands, and setting `[Console]::OutputEncoding` before PowerShell commands | Rewritten. Each command gets its own hidden console, so the user's console keeps its code page; PowerShell also gets `$OutputEncoding` and quiet progress |
| `internal/tools/tool.go`, `internal/tools/*.go` | `packages/core/src/tools/tool-names.ts` and the tool files | The tool and parameter names that models trained on Qwen Code send (`run_shell_command`, `replace`, `grep_search`, `search_file_content`, `file_path`, `absolute_path`, `dir_path`), accepted as aliases | Names only |
| `internal/prompt/compression.md` | `packages/core/src/core/prompts.ts` (`getCompressionPrompt`) | The compaction prompt: the summarizer's role, the `<analysis>` drafting block, and the `<state_snapshot>` structure with its sections and their guidance | The mention of restored file attachments, which this program doesn't add, was removed, and the prompt also covers compaction the user asks for. A sentence asking for brief drafting is new: on long conversations the drafting filled the reply before the summary |
| `internal/prompt/compression_request.md`, `compression_resume.md`, `compression_ack.md` | `packages/core/src/services/chatCompressionService.ts` (`COMPRESSION_REQUEST_DIRECTIVE`), `packages/core/src/services/postCompactAttachments.ts` (`RESUME_TRAILER`, the acknowledgement) | The request that ends the conversation sent to the summarizer, the trailer that tells the model to resume without acknowledging the summary, and the model's acknowledgement that keeps the turns alternating | A header line before the summary is new; a focus line is added when the user gives `/compact` instructions |
| `internal/agent/compact.go` | `packages/core/src/services/chatCompressionService.ts`, `packages/core/src/services/postCompactAttachments.ts` | The output budget for a summary (at most 20,000 tokens, clamped to the room left in the context window less a 1,024-token margin), stripping `<analysis>` blocks including one left unclosed, replacing the history with the summary and an acknowledgement, and the inflation guard: a summary no smaller than the conversation is dropped | Rewritten in Go. The whole conversation is summarized, and long tool results are shortened only when the conversation wouldn't otherwise fit; the default threshold is 180,000 tokens, capped at 85% of the context window |
| `internal/prompt/subagent_full.md`, `internal/prompt/subagent_read.md` | `packages/core/src/subagents/builtin-agents.ts` (the general-purpose and Explore agents) | The subagents' system prompts: a subagent works only on its task, inspects before changing, searches broad then narrow, verifies and reports uncertainty, and ends with a report for the parent naming files, checks and open issues; the read-only one searches with glob, grep and read_file, makes independent calls at once, and quotes code only when the exact text matters | Reworded for this program's tools: the read-only subagent has no shell, so its rules about read-only commands are gone; paths are relative to the workspace. The opening lines that gave the subagent a role ("You are a subagent that…", "You excel at…") were dropped on 2026-10-30: the prompts state the task, not a persona |
| `internal/prompt/tools/task.md` | `packages/core/src/tools/agent/agent.ts` (the Agent tool's description) | When not to start a subagent (a known path, a specific definition, two or three known files), and the usage notes: a short description, bounded independent tasks, no duplicated work, side-by-side subagents only for independent tasks with separate write scopes, relaying the report, and treating it as evidence | Rewritten for this program's task tool: no agent types, background agents, forks or worktrees; the read-only and full tool choices are new |

## Chintzy CPU font (the logo)

The logo in the startup banner (`internal/ui/logo.txt`) is the word "debbie" after the font
Chintzy CPU, created by Brian Kent (Ænigma Fonts, http://www.aenigmafonts.com/), drawn by hand
at 6 pixels high from the font's letterforms and stored as a bitmap. The same drawing is the
browser extension's wordmark (`extension/shared.js`) and, its first letter, the extension's
icons (`extension/icons/`, drawn by `scripts/icons.go`). The font itself is not
included or distributed. Its license text says: "All of my Fonts are Freeware, you can use
them any way you want to (Personal use, Commercial use, or whatever)."

## Apache License 2.0

```text
                                 Apache License
                           Version 2.0, January 2004
                        http://www.apache.org/licenses/

   TERMS AND CONDITIONS FOR USE, REPRODUCTION, AND DISTRIBUTION

   1. Definitions.

      "License" shall mean the terms and conditions for use, reproduction,
      and distribution as defined by Sections 1 through 9 of this document.

      "Licensor" shall mean the copyright owner or entity authorized by
      the copyright owner that is granting the License.

      "Legal Entity" shall mean the union of the acting entity and all
      other entities that control, are controlled by, or are under common
      control with that entity. For the purposes of this definition,
      "control" means (i) the power, direct or indirect, to cause the
      direction or management of such entity, whether by contract or
      otherwise, or (ii) ownership of fifty percent (50%) or more of the
      outstanding shares, or (iii) beneficial ownership of such entity.

      "You" (or "Your") shall mean an individual or Legal Entity
      exercising permissions granted by this License.

      "Source" form shall mean the preferred form for making modifications,
      including but not limited to software source code, documentation
      source, and configuration files.

      "Object" form shall mean any form resulting from mechanical
      transformation or translation of a Source form, including but
      not limited to compiled object code, generated documentation,
      and conversions to other media types.

      "Work" shall mean the work of authorship, whether in Source or
      Object form, made available under the License, as indicated by a
      copyright notice that is included in or attached to the work
      (an example is provided in the Appendix below).

      "Derivative Works" shall mean any work, whether in Source or Object
      form, that is based on (or derived from) the Work and for which the
      editorial revisions, annotations, elaborations, or other modifications
      represent, as a whole, an original work of authorship. For the purposes
      of this License, Derivative Works shall not include works that remain
      separable from, or merely link (or bind by name) to the interfaces of,
      the Work and Derivative Works thereof.

      "Contribution" shall mean any work of authorship, including
      the original version of the Work and any modifications or additions
      to that Work or Derivative Works thereof, that is intentionally
      submitted to Licensor for inclusion in the Work by the copyright owner
      or by an individual or Legal Entity authorized to submit on behalf of
      the copyright owner. For the purposes of this definition, "submitted"
      means any form of electronic, verbal, or written communication sent
      to the Licensor or its representatives, including but not limited to
      communication on electronic mailing lists, source code control systems,
      and issue tracking systems that are managed by, or on behalf of, the
      Licensor for the purpose of discussing and improving the Work, but
      excluding communication that is conspicuously marked or otherwise
      designated in writing by the copyright owner as "Not a Contribution."

      "Contributor" shall mean Licensor and any individual or Legal Entity
      on behalf of whom a Contribution has been received by Licensor and
      subsequently incorporated within the Work.

   2. Grant of Copyright License. Subject to the terms and conditions of
      this License, each Contributor hereby grants to You a perpetual,
      worldwide, non-exclusive, no-charge, royalty-free, irrevocable
      copyright license to reproduce, prepare Derivative Works of,
      publicly display, publicly perform, sublicense, and distribute the
      Work and such Derivative Works in Source or Object form.

   3. Grant of Patent License. Subject to the terms and conditions of
      this License, each Contributor hereby grants to You a perpetual,
      worldwide, non-exclusive, no-charge, royalty-free, irrevocable
      (except as stated in this section) patent license to make, have made,
      use, offer to sell, sell, import, and otherwise transfer the Work,
      where such license applies only to those patent claims licensable
      by such Contributor that are necessarily infringed by their
      Contribution(s) alone or by combination of their Contribution(s)
      with the Work to which such Contribution(s) was submitted. If You
      institute patent litigation against any entity (including a
      cross-claim or counterclaim in a lawsuit) alleging that the Work
      or a Contribution incorporated within the Work constitutes direct
      or contributory patent infringement, then any patent licenses
      granted to You under this License for that Work shall terminate
      as of the date such litigation is filed.

   4. Redistribution. You may reproduce and distribute copies of the
      Work or Derivative Works thereof in any medium, with or without
      modifications, and in Source or Object form, provided that You
      meet the following conditions:

      (a) You must give any other recipients of the Work or
          Derivative Works a copy of this License; and

      (b) You must cause any modified files to carry prominent notices
          stating that You changed the files; and

      (c) You must retain, in the Source form of any Derivative Works
          that You distribute, all copyright, patent, trademark, and
          attribution notices from the Source form of the Work,
          excluding those notices that do not pertain to any part of
          the Derivative Works; and

      (d) If the Work includes a "NOTICE" text file as part of its
          distribution, then any Derivative Works that You distribute must
          include a readable copy of the attribution notices contained
          within such NOTICE file, excluding those notices that do not
          pertain to any part of the Derivative Works, in at least one
          of the following places: within a NOTICE text file distributed
          as part of the Derivative Works; within the Source form or
          documentation, if provided along with the Derivative Works; or,
          within a display generated by the Derivative Works, if and
          wherever such third-party notices normally appear. The contents
          of the NOTICE file are for informational purposes only and
          do not modify the License. You may add Your own attribution
          notices within Derivative Works that You distribute, alongside
          or as an addendum to the NOTICE text from the Work, provided
          that such additional attribution notices cannot be construed
          as modifying the License.

      You may add Your own copyright statement to Your modifications and
      may provide additional or different license terms and conditions
      for use, reproduction, or distribution of Your modifications, or
      for any such Derivative Works as a whole, provided Your use,
      reproduction, and distribution of the Work otherwise complies with
      the conditions stated in this License.

   5. Submission of Contributions. Unless You explicitly state otherwise,
      any Contribution intentionally submitted for inclusion in the Work
      by You to the Licensor shall be under the terms and conditions of
      this License, without any additional terms or conditions.
      Notwithstanding the above, nothing herein shall supersede or modify
      the terms of any separate license agreement you may have executed
      with Licensor regarding such Contributions.

   6. Trademarks. This License does not grant permission to use the trade
      names, trademarks, service marks, or product names of the Licensor,
      except as required for reasonable and customary use in describing the
      origin of the Work and reproducing the content of the NOTICE file.

   7. Disclaimer of Warranty. Unless required by applicable law or
      agreed to in writing, Licensor provides the Work (and each
      Contributor provides its Contributions) on an "AS IS" BASIS,
      WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
      implied, including, without limitation, any warranties or conditions
      of TITLE, NON-INFRINGEMENT, MERCHANTABILITY, or FITNESS FOR A
      PARTICULAR PURPOSE. You are solely responsible for determining the
      appropriateness of using or redistributing the Work and assume any
      risks associated with Your exercise of permissions under this License.

   8. Limitation of Liability. In no event and under no legal theory,
      whether in tort (including negligence), contract, or otherwise,
      unless required by applicable law (such as deliberate and grossly
      negligent acts) or agreed to in writing, shall any Contributor be
      liable to You for damages, including any direct, indirect, special,
      incidental, or consequential damages of any character arising as a
      result of this License or out of the use or inability to use the
      Work (including but not limited to damages for loss of goodwill,
      work stoppage, computer failure or malfunction, or any and all
      other commercial damages or losses), even if such Contributor
      has been advised of the possibility of such damages.

   9. Accepting Warranty or Additional Liability. While redistributing
      the Work or Derivative Works thereof, You may choose to offer,
      and charge a fee for, acceptance of support, warranty, indemnity,
      or other liability obligations and/or rights consistent with this
      License. However, in accepting such obligations, You may act only
      on Your own behalf and on Your sole responsibility, not on behalf
      of any other Contributor, and only if You agree to indemnify,
      defend, and hold each Contributor harmless for any liability
      incurred by, or claims asserted against, such Contributor by reason
      of your accepting any such warranty or additional liability.

   END OF TERMS AND CONDITIONS

   APPENDIX: How to apply the Apache License to your work.

      To apply the Apache License to your work, attach the following
      boilerplate notice, with the fields enclosed by brackets "[]"
      replaced with your own identifying information. (Don't include
      the brackets!)  The text should be enclosed in the appropriate
      comment syntax for the file format. We also recommend that a
      file or class name and description of purpose be included on the
      same "printed page" as the copyright notice for easier
      identification within third-party archives.

  Copyright 2025 Google LLC
  Copyright 2025 Qwen

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
```
