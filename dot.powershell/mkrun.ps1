function mk-run-alias {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory = $true)]
        [string]$Name,

        [Parameter(Mandatory = $true)]
        [string]$ExePath
    )

    # we build the function text so PS can see a real function body
    $fn = @"
function $Name {
    param(
        [Parameter(ValueFromRemainingArguments = \$true)]
        [string[]]\$Args
    )
    run '$ExePath' @Args
}
"@

    # define it in global scope so it's immediately available + completable
    Invoke-Expression $fn
}
