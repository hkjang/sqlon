import os
import re
from markdown_it import MarkdownIt
from playwright.sync_api import sync_playwright

# Define paths
REPORTS_DIR = os.path.dirname(os.path.abspath(__file__))
WORKSPACE_DIR = os.path.abspath(os.path.join(REPORTS_DIR, "..", ".."))

# CSS Stylesheet (Premium documentation theme)
CSS_STYLE = """
@import url('https://fonts.googleapis.com/css2?family=Noto+Sans+KR:wght@300;400;500;700&family=Fira+Code:wght@400;500&display=swap');

body {
    font-family: 'Noto Sans KR', 'Malgun Gothic', 'Apple SD Gothic Neo', sans-serif;
    line-height: 1.8;
    color: #334155;
    background-color: #ffffff;
    margin: 0;
    padding: 0;
}

.container {
    max-width: 840px;
    margin: 0 auto;
    padding: 50px;
}

/* Cover page style for A4 PDF */
.cover-page {
    height: 100vh;
    display: flex;
    flex-direction: column;
    justify-content: space-between;
    box-sizing: border-box;
    padding: 100px 50px;
    border-bottom: 2px solid #e2e8f0;
    page-break-after: always;
}

.cover-header {
    font-size: 14px;
    font-weight: 700;
    color: #4f46e5;
    text-transform: uppercase;
    letter-spacing: 2px;
}

.cover-body {
    margin-top: 100px;
    flex-grow: 1;
}

.cover-title {
    font-size: 34px;
    font-weight: 700;
    color: #0f172a;
    line-height: 1.3;
    margin-bottom: 20px;
    word-break: keep-all;
}

.cover-subtitle {
    font-size: 18px;
    color: #475569;
    font-weight: 400;
    margin-bottom: 50px;
    word-break: keep-all;
}

.cover-footer {
    border-top: 2px solid #f1f5f9;
    padding-top: 30px;
}

.cover-meta-table {
    width: 100%;
    border-collapse: collapse;
    margin: 0;
}

.cover-meta-table td {
    padding: 8px 0;
    border: none;
    font-size: 14px;
    color: #475569;
}

.cover-meta-table td.label {
    font-weight: 700;
    color: #0f172a;
    width: 120px;
}

/* Typography */
h1 {
    font-size: 26px;
    font-weight: 700;
    color: #0f172a;
    margin-top: 50px;
    margin-bottom: 24px;
    border-bottom: 2px solid #f1f5f9;
    padding-bottom: 10px;
    page-break-before: always;
}

h1:first-of-type {
    page-break-before: avoid;
}

h2 {
    font-size: 20px;
    font-weight: 700;
    color: #1e293b;
    margin-top: 36px;
    margin-bottom: 18px;
}

h3 {
    font-size: 16px;
    font-weight: 700;
    color: #334155;
    margin-top: 28px;
    margin-bottom: 12px;
}

p {
    margin-top: 0;
    margin-bottom: 18px;
    text-align: justify;
    word-break: keep-all;
}

/* Lists */
ul, ol {
    margin-top: 0;
    margin-bottom: 18px;
    padding-left: 24px;
}

li {
    margin-bottom: 8px;
}

/* Tables */
table {
    width: 100%;
    border-collapse: collapse;
    margin-top: 20px;
    margin-bottom: 28px;
    font-size: 14px;
}

th, td {
    padding: 12px 16px;
    border: 1px solid #cbd5e1;
    text-align: left;
}

th {
    background-color: #f1f5f9;
    color: #0f172a;
    font-weight: 700;
}

tr:nth-child(even) {
    background-color: #f8fafc;
}

/* Code & Pre */
code {
    font-family: 'Fira Code', 'Consolas', monospace;
    font-size: 13px;
    background-color: #f1f5f9;
    color: #0f172a;
    padding: 3px 6px;
    border-radius: 4px;
}

pre {
    background-color: #0f172a;
    color: #f8fafc;
    padding: 18px;
    border-radius: 8px;
    overflow-x: auto;
    margin-top: 20px;
    margin-bottom: 28px;
}

pre code {
    background-color: transparent;
    color: inherit;
    padding: 0;
    font-size: 13px;
}

/* Alerts */
.alert {
    padding: 16px 20px;
    border-left: 5px solid #cbd5e1;
    background-color: #f8fafc;
    border-radius: 0 8px 8px 0;
    margin-top: 20px;
    margin-bottom: 28px;
}

.alert p:last-child {
    margin-bottom: 0;
}

.alert-note {
    border-left-color: #3b82f6;
    background-color: #eff6ff;
}

.alert-tip {
    border-left-color: #10b981;
    background-color: #ecfdf5;
}

.alert-important {
    border-left-color: #6366f1;
    background-color: #eef2ff;
}

.alert-warning {
    border-left-color: #f59e0b;
    background-color: #fffbeb;
}

.alert-caution {
    border-left-color: #ef4444;
    background-color: #fef2f2;
}

.alert-title {
    font-weight: 700;
    margin-bottom: 8px;
    color: #0f172a;
    text-transform: uppercase;
    font-size: 12px;
    letter-spacing: 1px;
}

/* Dividers */
hr {
    border: 0;
    border-top: 1px solid #e2e8f0;
    margin: 40px 0;
}

/* Mermaid Diagrams */
.mermaid {
    display: flex;
    justify-content: center;
    margin: 30px 0;
    background-color: #f8fafc;
    padding: 20px;
    border-radius: 8px;
    border: 1px solid #e2e8f0;
}

/* Print optimizations */
@media print {
    body {
        padding: 0;
        font-size: 11pt;
        -webkit-print-color-adjust: exact;
        print-color-adjust: exact;
    }
    
    .container {
        padding: 0;
    }
    
    h1, h2, h3 {
        page-break-after: avoid;
    }
    
    tr, pre, blockquote, .alert, .mermaid {
        page-break-inside: avoid;
    }
}
"""

def replace_alerts(html):
    # Robustly converts blockquotes with [!TYPE] into div alerts using BeautifulSoup
    from bs4 import BeautifulSoup
    soup = BeautifulSoup(html, "html.parser")
    
    for bq in soup.find_all("blockquote"):
        first_p = bq.find("p")
        if first_p:
            # Check if text starts with [!TYPE]
            text = first_p.get_text().strip()
            match = re.match(r"^\[!(NOTE|TIP|IMPORTANT|WARNING|CAUTION)\]", text, re.IGNORECASE)
            if match:
                alert_type = match.group(1).upper()
                
                # Convert tag blockquote -> div
                bq.name = "div"
                bq["class"] = f"alert alert-{alert_type.lower()}"
                
                # Add alert title
                title_div = soup.new_tag("div")
                title_div["class"] = "alert-title"
                title_div.string = alert_type
                bq.insert(0, title_div)
                
                # Remove [!TYPE] prefix from the text content of the first paragraph
                # Iterate through children to find the first text node and clean it
                for child in first_p.children:
                    if isinstance(child, str):
                        # Text node
                        new_text = re.sub(r"^\[!(NOTE|TIP|IMPORTANT|WARNING|CAUTION)\]\s*", "", child, flags=re.IGNORECASE)
                        child.replace_with(new_text)
                        break
                    elif hasattr(child, "string") and child.string:
                        new_text = re.sub(r"^\[!(NOTE|TIP|IMPORTANT|WARNING|CAUTION)\]\s*", "", child.string, flags=re.IGNORECASE)
                        child.string = new_text
                        break
                        
    return str(soup)

def replace_mermaid(html):
    # Matches code blocks rendered as:
    # <pre><code class="language-mermaid">...</code></pre>
    # We will convert it into a <div class="mermaid">...</div> for rendering with mermaid.js.
    pattern = r'<pre><code class="language-mermaid">(.*?)</code></pre>'
    return re.sub(pattern, r'<div class="mermaid">\1</div>', html, flags=re.DOTALL)

def wrap_html(body_html, title, subtitle, version, date, department):
    cover_page = f"""
    <div class="cover-page">
        <div class="cover-header">{department} 기술 보고서</div>
        <div class="cover-body">
            <h1 class="cover-title">{title}</h1>
            <p class="cover-subtitle">{subtitle}</p>
        </div>
        <div class="cover-footer">
            <table class="cover-meta-table">
                <tr>
                    <td class="label">작성 부서</td>
                    <td>{department}</td>
                </tr>
                <tr>
                    <td class="label">발행 일자</td>
                    <td>{date}</td>
                </tr>
                <tr>
                    <td class="label">문서 버전</td>
                    <td>{version}</td>
                </tr>
                <tr>
                    <td class="label">보안 구분</td>
                    <td>대외비 (Confidential)</td>
                </tr>
            </table>
        </div>
    </div>
    """
    
    html = f"""<!DOCTYPE html>
<html lang="ko">
<head>
    <meta charset="UTF-8">
    <title>{title}</title>
    <style>
        {CSS_STYLE}
    </style>
    <!-- Load Mermaid.js for rendering charts -->
    <script type="module">
        import mermaid from 'https://cdn.jsdelivr.net/npm/mermaid@10/dist/mermaid.esm.min.mjs';
        mermaid.initialize({{
            startOnLoad: true,
            theme: 'default',
            flowchart: {{ useMaxWidth: true, htmlLabels: true }}
        }});
    </script>
</head>
<body>
    <div class="container">
        {cover_page}
        <div class="document-content">
            {body_html}
        </div>
    </div>
</body>
</html>
"""
    return html

def convert_md_to_html_and_pdf(filename, title, subtitle, version, date, department):
    md_path = os.path.join(REPORTS_DIR, f"{filename}.md")
    html_path = os.path.join(REPORTS_DIR, f"{filename}.html")
    pdf_path = os.path.join(REPORTS_DIR, f"{filename}.pdf")
    
    print(f"Processing {filename}.md...")
    
    # Read Markdown content
    with open(md_path, "r", encoding="utf-8") as f:
        md_text = f.read()
    
    # Remove H1 markdown header (which is used as the document title on the cover page)
    # The title markdown starts with '# [임원 보고서] ...' or similar. We want to clean it up for the body.
    lines = md_text.splitlines()
    if lines and lines[0].startswith("#"):
        lines = lines[1:]
    body_md = "\n".join(lines)
    
    # Render to HTML
    md = MarkdownIt()
    md.enable("table")
    body_html = md.render(body_md)
    
    # Replace blockquotes with custom alert divs
    body_html = replace_alerts(body_html)
    
    # Replace pre code with mermaid divs
    body_html = replace_mermaid(body_html)
    
    # Wrap in complete HTML template
    full_html = wrap_html(body_html, title, subtitle, version, date, department)
    
    # Save HTML to file
    with open(html_path, "w", encoding="utf-8") as f:
        f.write(full_html)
    
    print(f"Generated HTML: {html_path}")
    
    # Generate PDF using Playwright
    with sync_playwright() as p:
        # Launch browser
        browser = p.chromium.launch()
        page = browser.new_page()
        
        # Load local HTML file
        page.goto(f"file:///{html_path.replace(os.sep, '/')}")
        
        # Wait for mermaid rendering if any .mermaid elements exist
        if "<div class=\"mermaid\"" in full_html:
            print("Mermaid diagram detected. Waiting for render...")
            page.wait_for_timeout(2000) # Give mermaid 2 seconds to fetch library and render SVG
        
        # Export to PDF with premium format
        page.pdf(
            path=pdf_path,
            format="A4",
            print_background=True,
            margin={
                "top": "20mm",
                "bottom": "20mm",
                "left": "20mm",
                "right": "20mm"
            },
            display_header_footer=True,
            header_template='<div style="font-size: 8px; color: #94a3b8; width: 100%; text-align: right; padding-right: 20mm; font-family: sans-serif;">SQLON NL2SQL & MCP 엔터프라이즈 문서 - AI 인프라실</div>',
            footer_template='<div style="font-size: 8px; color: #94a3b8; width: 100%; text-align: center; font-family: sans-serif;"><span class="pageNumber"></span> / <span class="totalPages"></span></div>'
        )
        
        browser.close()
        
    print(f"Generated PDF: {pdf_path}")

if __name__ == "__main__":
    # 1. Executive Report
    convert_md_to_html_and_pdf(
        filename="executive_report",
        title="SQLON NL2SQL 엔터프라이즈 시스템 도입 성과 및 전략 로드맵 보고서",
        subtitle="자연어 기반 데이터 접근성 혁신, 데이터 보안 가드레일 확보 및 데이터 드리븐 조직 전환 성과 (PostgreSQL·MySQL·MariaDB·Oracle)",
        version="v0.1.2",
        date="2026년 7월 20일",
        department="AI 인프라실 & 데이터 가버넌스팀"
    )
    
    # 2. User Guide
    convert_md_to_html_and_pdf(
        filename="user_guide",
        title="SQLON NL2SQL & MCP 엔터프라이즈 사용자 가이드",
        subtitle="자연어-SQL 변환, 메타데이터 탐색 및 안전 실행 모니터링 사용자 지침서",
        version="v0.1.2",
        date="2026년 7월 20일",
        department="AI 인프라실"
    )
    
    # 3. Admin Guide
    convert_md_to_html_and_pdf(
        filename="admin_guide",
        title="SQLON 시스템 관리자 및 DBA 가이드",
        subtitle="설치 배포, 오프라인 폐쇄망 운영, DB 연동, 보안 및 플릿 관측성 구축 지침서",
        version="v0.1.2",
        date="2026년 7월 20일",
        department="AI 인프라실 & DBA팀"
    )
    
    # 4. MCP Analysis
    convert_md_to_html_and_pdf(
        filename="mcp_analysis",
        title="SQLON MCP (Model Context Protocol) 툴셋 및 작동 원리 분석서",
        subtitle="자연어 질의 인터페이스(MCP) 전송 계층 및 28종 도구 심층 설계 분석",
        version="v0.1.2",
        date="2026년 7월 20일",
        department="AI 인프라실"
    )
    
    print("All documents generated successfully in HTML, MD, and PDF formats!")
