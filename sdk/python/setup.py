from setuptools import find_packages, setup

with open("README.md", "r", encoding="utf-8") as f:
    try:
        long_description = f.read()
    except FileNotFoundError:
        long_description = "Event Analytics SDK - Python Client"

setup(
    name="event-analytics-sdk",
    version="0.1.0",
    author="krxsna",
    description="Python SDK for the Event Analytics Pipeline API",
    long_description=long_description,
    long_description_content_type="text/markdown",
    url="https://github.com/krxsna/agnostai-task",
    py_modules=["event_client"],
    python_requires=">=3.9",
    install_requires=[],
    extras_require={
        "dev": [
            "pytest>=7.0",
            "pytest-cov>=4.0",
        ],
    },
    classifiers=[
        "Development Status :: 3 - Alpha",
        "Intended Audience :: Developers",
        "License :: OSI Approved :: MIT License",
        "Programming Language :: Python :: 3",
        "Programming Language :: Python :: 3.9",
        "Programming Language :: Python :: 3.10",
        "Programming Language :: Python :: 3.11",
        "Programming Language :: Python :: 3.12",
        "Operating System :: OS Independent",
        "Topic :: Software Development :: Libraries :: Python Modules",
    ],
    keywords="event analytics tracking sdk pipeline kafka clickhouse",
    project_urls={
        "Bug Reports": "https://github.com/krxsna/agnostai-task/issues",
        "Source": "https://github.com/krxsna/agnostai-task",
    },
)
