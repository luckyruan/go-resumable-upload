#Requires -Version 7.0
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$FilePath,
    [string]$BaseUrl = 'http://localhost:8092',
    [string]$ApiKey = 'local-upload-key',
    [string]$UploadId,
    [int]$ChunkSize = 1048576
)

$ErrorActionPreference = 'Stop'
$resolvedPath = (Resolve-Path -LiteralPath $FilePath).Path
$source = Get-Item -LiteralPath $resolvedPath
if ($source.PSIsContainer -or $source.Length -le 0) {
    throw 'FilePath must identify a non-empty regular file.'
}
$headers = @{ 'X-API-Key' = $ApiKey }
$base = $BaseUrl.TrimEnd('/')
$stream = [System.IO.File]::OpenRead($resolvedPath)
try {
    $hasher = [System.Security.Cryptography.SHA256]::Create()
    try {
        $wholeHash = [BitConverter]::ToString($hasher.ComputeHash($stream)).Replace('-', '').ToLowerInvariant()
    } finally {
        $hasher.Dispose()
    }
    if ([string]::IsNullOrWhiteSpace($UploadId)) {
        $payload = @{
            filename = $source.Name
            size = $source.Length
            chunkSize = $ChunkSize
            sha256 = $wholeHash
        } | ConvertTo-Json
        $upload = Invoke-RestMethod -Method Post -Uri "$base/api/v1/uploads" `
            -Headers $headers -ContentType 'application/json' -Body $payload
        $UploadId = $upload.id
        Write-Host "UploadId: $UploadId (save this ID before interrupting the client)"
    } else {
        if ($UploadId -cnotmatch '^[0-9a-f]{32}$') { throw 'Invalid UploadId.' }
        $upload = Invoke-RestMethod -Method Get -Uri "$base/api/v1/uploads/$UploadId" -Headers $headers
        if ($upload.size -ne $source.Length -or $upload.sha256 -ne $wholeHash) {
            throw 'Local file differs from the original upload declaration.'
        }
        $ChunkSize = [int]$upload.chunkSize
    }
    if ($upload.status -eq 'CANCELED') { throw 'Upload is canceled; create a new upload.' }
    if ($upload.status -eq 'COMPLETED') {
        Write-Host "Already complete: $base$($upload.downloadPath)"
        return
    }
    $committed = [System.Collections.Generic.HashSet[int]]::new()
    foreach ($part in $upload.uploadedParts) { [void]$committed.Add([int]$part.partNumber) }
    for ($number = 1; $number -le $upload.totalParts; $number++) {
        if ($committed.Contains($number)) {
            Write-Host "Skipping committed part $number"
            continue
        }
        $offset = [long]($number - 1) * [long]$ChunkSize
        $count = [int][Math]::Min([long]$ChunkSize, $source.Length - $offset)
        [void]$stream.Seek($offset, [System.IO.SeekOrigin]::Begin)
        $bytes = [byte[]]::new($count)
        $read = 0
        while ($read -lt $count) {
            $received = $stream.Read($bytes, $read, $count - $read)
            if ($received -eq 0) { throw 'Unexpected end of local file.' }
            $read += $received
        }
        $partHasher = [System.Security.Cryptography.SHA256]::Create()
        try {
            $partHash = [BitConverter]::ToString($partHasher.ComputeHash($bytes)).Replace('-', '').ToLowerInvariant()
        } finally {
            $partHasher.Dispose()
        }
        $partHeaders = @{ 'X-API-Key' = $ApiKey; 'X-Chunk-SHA256' = $partHash }
        $result = Invoke-RestMethod -Method Put -Uri "$base/api/v1/uploads/$UploadId/parts/$number" `
            -Headers $partHeaders -ContentType 'application/octet-stream' -Body $bytes
        Write-Host "Committed part $($result.partNumber), size=$($result.size)"
    }
    $completed = Invoke-RestMethod -Method Post -Uri "$base/api/v1/uploads/$UploadId/complete" `
        -Headers $headers -TimeoutSec 1800
    Write-Host "Complete: $base$($completed.downloadPath)"
    Write-Host 'Downloading requires the same X-API-Key header.'
} finally {
    $stream.Dispose()
}
