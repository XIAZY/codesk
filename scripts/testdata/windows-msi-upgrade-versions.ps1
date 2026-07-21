Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

# These synthetic versions exist only to exercise MSI upgrade behavior. The
# production artifact path never reads this fixture.
@(
    [pscustomobject] @{
        name = 'previous'
        version = '0.0.1'
    },
    [pscustomobject] @{
        name = 'candidate'
        version = '0.0.2'
    }
)
