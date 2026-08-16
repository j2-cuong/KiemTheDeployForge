This directory carries the Setup GUI stub that the Builder embeds.

SetupStub.exe is a build output, not source. scripts\Build-Tools.ps1 compiles
cmd\setup, verifies its PE header, audits it and only then publishes it here,
so it is deliberately left out of version control.

This placeholder is tracked so that the //go:embed pattern in main.go always
matches at least one file. Without it a fresh clone cannot compile at all, and
the failure ("pattern assets/*: no matching files found") says nothing about
what the reader is actually missing.

When SetupStub.exe is absent the Builder still compiles and starts; pressing
the build button reports that the stub is missing and to run
scripts\Build-Tools.ps1.
