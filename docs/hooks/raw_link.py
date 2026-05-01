REPO_RAW = 'https://raw.githubusercontent.com/dlorenc/docstore/main'

def on_page_content(html, page, config, files):
    src = page.file.src_path
    url = f'{REPO_RAW}/docs/{src}'
    link = f'<hr><p><small><a href="{url}">Raw markdown</a> — machine-readable source for this page.</small></p>'
    return html + link
