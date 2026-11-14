# Картинки установщика NSIS (Windows PowerShell 5.1, System.Drawing).
# Запуск из корня репозитория: powershell -ExecutionPolicy Bypass -File desktop/installer/make-images.ps1
#   sidebar.bmp 164x314 — левая панель на первой и последней странице установщика;
#   header.bmp  150x57  — шапка остальных страниц, слева от заголовка (фон шапки у NSIS белый).
Add-Type -AssemblyName System.Drawing

$dir = Split-Path -Parent $MyInvocation.MyCommand.Path
$logo = [System.Drawing.Image]::FromFile((Join-Path $dir '..\src-tauri\icons\icon.png'))

$cyan = [System.Drawing.Color]::FromArgb(34, 211, 238)
$indigo = [System.Drawing.Color]::FromArgb(99, 102, 241)
$purple = [System.Drawing.Color]::FromArgb(168, 85, 247)

function New-Canvas($w, $h) {
    $bmp = New-Object System.Drawing.Bitmap($w, $h, [System.Drawing.Imaging.PixelFormat]::Format24bppRgb)
    $g = [System.Drawing.Graphics]::FromImage($bmp)
    $g.SmoothingMode = 'AntiAlias'
    $g.InterpolationMode = 'HighQualityBicubic'
    $g.PixelOffsetMode = 'HighQuality'
    $g.TextRenderingHint = 'AntiAliasGridFit'
    return $bmp, $g
}

# градиент cyan → indigo → purple слева направо
function New-BrandBrush($x, $y, $w, $h) {
    $rect = New-Object System.Drawing.RectangleF($x, $y, $w, $h)
    $b = New-Object System.Drawing.Drawing2D.LinearGradientBrush($rect, $cyan, $purple, [single]0)
    $blend = New-Object System.Drawing.Drawing2D.ColorBlend(3)
    $blend.Colors = @($cyan, $indigo, $purple)
    $blend.Positions = @([single]0, [single]0.5, [single]1)
    $b.InterpolationColors = $blend
    return $b
}

function Add-RoundRect($path, $x, $y, $w, $h, $r) {
    $r = [Math]::Min($r, [Math]::Min($w, $h) / 2)
    $d = $r * 2
    if ($d -le 0) { $path.AddRectangle((New-Object System.Drawing.RectangleF($x, $y, $w, $h))); return }
    $path.AddArc($x, $y, $d, $d, 180, 90)
    $path.AddArc($x + $w - $d, $y, $d, $d, 270, 90)
    $path.AddArc($x + $w - $d, $y + $h - $d, $d, $d, 0, 90)
    $path.AddArc($x, $y + $h - $d, $d, $d, 90, 90)
    $path.CloseFigure()
}

# «звуковая волна» из столбиков, по центру cy
function Draw-Wave($g, $x, $cy, $w, $maxH, $count, $alpha) {
    $gap = $w / $count
    $barW = [Math]::Max(2, $gap * 0.55)
    $path = New-Object System.Drawing.Drawing2D.GraphicsPath
    for ($i = 0; $i -lt $count; $i++) {
        $t = $i / [Math]::Max(1, $count - 1)
        $env = [Math]::Sin([Math]::PI * $t)
        $v = 0.35 + 0.65 * [Math]::Abs([Math]::Sin($i * 1.7) * [Math]::Cos($i * 0.6))
        $bh = [Math]::Max($barW, $maxH * $env * $v)
        Add-RoundRect $path ($x + $i * $gap) ($cy - $bh / 2) $barW $bh ($barW / 2)
    }
    $brush = New-BrandBrush $x 0 $w 1
    $g.FillPath($brush, $path)
    if ($alpha -lt 255) {
        # приглушаем волну полупрозрачной заливкой фона поверх
        $g.FillPath((New-Object System.Drawing.SolidBrush([System.Drawing.Color]::FromArgb(255 - $alpha, $script:waveBg))), $path)
    }
}

# ---------- sidebar 164x314 ----------
$bmp, $g = New-Canvas 164 314
$bgTop = [System.Drawing.Color]::FromArgb(30, 31, 34)
$bgBottom = [System.Drawing.Color]::FromArgb(20, 21, 26)
$bg = New-Object System.Drawing.Drawing2D.LinearGradientBrush((New-Object System.Drawing.Rectangle(0, 0, 164, 314)), $bgTop, $bgBottom, [single]90)
$g.FillRectangle($bg, 0, 0, 164, 314)

# мягкое свечение за логотипом
$glow = New-Object System.Drawing.Drawing2D.GraphicsPath
$glow.AddEllipse(2, 22, 160, 160)
$pg = New-Object System.Drawing.Drawing2D.PathGradientBrush($glow)
$pg.CenterColor = [System.Drawing.Color]::FromArgb(90, $indigo)
$pg.SurroundColors = @([System.Drawing.Color]::FromArgb(0, $indigo))
$g.FillPath($pg, $glow)

$g.DrawImage($logo, 42, 62, 80, 80)

$fmt = New-Object System.Drawing.StringFormat
$fmt.Alignment = 'Center'
$title = New-Object System.Drawing.Font('Segoe UI', 15, [System.Drawing.FontStyle]::Bold, [System.Drawing.GraphicsUnit]::Pixel)
$g.DrawString('ZALUPA228', $title, (New-Object System.Drawing.SolidBrush([System.Drawing.Color]::FromArgb(242, 243, 245))), (New-Object System.Drawing.RectangleF(0, 156, 164, 24)), $fmt)
$sub = New-Object System.Drawing.Font('Segoe UI', 12, [System.Drawing.FontStyle]::Regular, [System.Drawing.GraphicsUnit]::Pixel)
$g.DrawString('голосовой чат', $sub, (New-Object System.Drawing.SolidBrush([System.Drawing.Color]::FromArgb(150, 155, 165))), (New-Object System.Drawing.RectangleF(0, 180, 164, 20)), $fmt)

$script:waveBg = $bgBottom
Draw-Wave $g 22 262 120 34 15 255

$g.Dispose()
$bmp.Save((Join-Path $dir 'sidebar.bmp'), [System.Drawing.Imaging.ImageFormat]::Bmp)
$bmp.Dispose()

# ---------- header 150x57 ----------
$bmp, $g = New-Canvas 150 57
$g.Clear([System.Drawing.Color]::White)
$script:waveBg = [System.Drawing.Color]::White
# NSIS ставит картинку шапки слева, заголовок страницы — правее: логотип у левого края, волна затухает к тексту
$g.DrawImage($logo, 7, 7, 43, 43)
Draw-Wave $g 58 28.5 84 28 11 200
$g.Dispose()
$bmp.Save((Join-Path $dir 'header.bmp'), [System.Drawing.Imaging.ImageFormat]::Bmp)
$bmp.Dispose()

$logo.Dispose()
'готово: sidebar.bmp, header.bmp'
